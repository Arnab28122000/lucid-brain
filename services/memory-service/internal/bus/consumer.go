// Package bus consumes ContentEvents from NATS JetStream and hands them to the
// memory pipeline.
package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/cortex-ai/cortex/services/memory-service/internal/contentevent"
	"github.com/cortex-ai/cortex/services/memory-service/internal/memory"
	"github.com/cortex-ai/cortex/services/memory-service/internal/pipeline"
	"github.com/cortex-ai/cortex/services/memory-service/internal/store"
)

// Handler processes one episode. The pipeline satisfies it; tests substitute a
// stub.
type Handler interface {
	Process(ctx context.Context, ep memory.Episode) (pipeline.Report, error)
}

// Deleter handles source deletions, which invalidate derived memories rather
// than dropping them.
type Deleter interface {
	InvalidateByDocument(ctx context.Context, tenantID, documentID string, at time.Time, purge bool) (int, error)
}

// Config configures the consumer.
type Config struct {
	URL         string
	Stream      string
	Subject     string
	Durable     string
	MaxInflight int
	// AckWait must exceed the worst-case extraction time or JetStream will
	// redeliver an episode that is still being processed — which is safe
	// (extraction is idempotent by content hash) but wasteful in exactly the
	// case where the system is already slow.
	AckWait       time.Duration
	MaxDeliver    int
	PurgeOnDelete bool
}

// Defaults fills in production-shaped values.
func (c *Config) Defaults() {
	if c.URL == "" {
		c.URL = nats.DefaultURL
	}
	if c.Stream == "" {
		c.Stream = contentevent.StreamName
	}
	if c.Subject == "" {
		c.Subject = contentevent.SubjectPattern
	}
	if c.Durable == "" {
		c.Durable = "memory-service"
	}
	if c.MaxInflight <= 0 {
		// Bounded on purpose. The backfill spike is what breaks ingestion, and
		// an unbounded consumer would forward the whole spike to the LLM
		// gateway, where it becomes GPU spend rather than queue depth.
		c.MaxInflight = 8
	}
	if c.AckWait <= 0 {
		c.AckWait = 5 * time.Minute
	}
	if c.MaxDeliver <= 0 {
		c.MaxDeliver = 5
	}
}

// Consumer is a durable pull consumer over the content stream.
type Consumer struct {
	cfg     Config
	log     *slog.Logger
	handler Handler
	deleter Deleter

	nc  *nats.Conn
	js  jetstream.JetStream
	cc  jetstream.ConsumeContext
	mu  sync.Mutex
	err error
}

// NewConsumer connects to NATS and binds the durable consumer.
func NewConsumer(ctx context.Context, cfg Config, h Handler, d Deleter, log *slog.Logger) (*Consumer, error) {
	cfg.Defaults()

	nc, err := nats.Connect(cfg.URL,
		nats.Name("cortex-memory-service"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			log.Warn("nats disconnected", "error", err)
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) { log.Info("nats reconnected") }),
	)
	if err != nil {
		return nil, fmt.Errorf("bus: connect: %w", err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("bus: jetstream: %w", err)
	}
	return &Consumer{cfg: cfg, log: log, handler: h, deleter: d, nc: nc, js: js}, nil
}

// Start binds the durable consumer and begins processing. It returns once the
// consumer is running; Stop tears it down.
func (c *Consumer) Start(ctx context.Context) error {
	stream, err := c.js.Stream(ctx, c.cfg.Stream)
	if err != nil {
		return fmt.Errorf("bus: stream %q: %w", c.cfg.Stream, err)
	}

	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       c.cfg.Durable,
		FilterSubject: c.cfg.Subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       c.cfg.AckWait,
		MaxDeliver:    c.cfg.MaxDeliver,
		MaxAckPending: c.cfg.MaxInflight,
		// DeliverAll, not DeliverNew: a memory service brought up after the
		// ingestion plane must build memory from everything already indexed,
		// and the ingest log makes replay cheap.
		DeliverPolicy: jetstream.DeliverAllPolicy,
		BackOff:       backoffFor(c.cfg.MaxDeliver),
	})
	if err != nil {
		return fmt.Errorf("bus: create consumer: %w", err)
	}

	cc, err := cons.Consume(func(msg jetstream.Msg) {
		c.handle(ctx, msg)
	}, jetstream.PullMaxMessages(c.cfg.MaxInflight))
	if err != nil {
		return fmt.Errorf("bus: consume: %w", err)
	}
	c.cc = cc
	c.log.Info("consuming content events",
		"stream", c.cfg.Stream, "subject", c.cfg.Subject, "durable", c.cfg.Durable,
		"max_inflight", c.cfg.MaxInflight)
	return nil
}

// defaultBackoff spreads redelivery out far enough that a cold LLM gateway has
// time to come back, rather than burning the delivery budget in the first
// minute of an outage.
var defaultBackoff = []time.Duration{
	5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute,
}

// backoffFor trims the schedule to fit the delivery budget. JetStream rejects a
// consumer whose backoff list is not shorter than MaxDeliver, and it rejects it
// at Start — so lowering MaxDeliver in a ConfigMap would otherwise take the
// service down at boot rather than merely retrying less.
func backoffFor(maxDeliver int) []time.Duration {
	if maxDeliver <= 1 {
		return nil
	}
	if len(defaultBackoff) >= maxDeliver {
		return defaultBackoff[:maxDeliver-1]
	}
	return defaultBackoff
}

func (c *Consumer) handle(ctx context.Context, msg jetstream.Msg) {
	// A per-message deadline below AckWait keeps a hung LLM call from holding
	// an inflight slot until JetStream redelivers underneath it.
	ctx, cancel := context.WithTimeout(ctx, c.cfg.AckWait-30*time.Second)
	defer cancel()

	ev, err := contentevent.Decode(msg.Data())
	if err != nil {
		// Undecodable payloads will never decode. Terminate rather than retry,
		// so one poison message cannot occupy a redelivery slot forever.
		c.log.Error("undecodable content event", "error", err)
		_ = msg.Term()
		return
	}

	if ev.Op == contentevent.OpDelete {
		if err := c.handleDelete(ctx, ev); err != nil {
			c.log.Error("delete propagation failed", "document_id", ev.DocumentID, "error", err)
			_ = msg.Nak()
			return
		}
		_ = msg.Ack()
		return
	}

	ep, err := ev.Episode()
	if errors.Is(err, contentevent.ErrSkip) {
		_ = msg.Ack()
		return
	}
	if err != nil {
		c.log.Error("invalid content event", "event_id", ev.EventID, "error", err)
		_ = msg.Term()
		return
	}

	if _, err := c.handler.Process(ctx, ep); err != nil {
		// Nak with backoff: extraction failures are usually the LLM gateway
		// being unavailable, which is transient. The ingest log is only written
		// on success, so redelivery reprocesses cleanly.
		c.log.Error("episode processing failed", "episode_id", ep.ID, "error", err)
		md, mdErr := msg.Metadata()
		if mdErr == nil && md.NumDelivered >= uint64(c.cfg.MaxDeliver) {
			c.log.Error("episode exhausted retries, terminating",
				"episode_id", ep.ID, "delivered", md.NumDelivered)
			_ = msg.Term()
			return
		}
		_ = msg.Nak()
		return
	}
	_ = msg.Ack()
}

func (c *Consumer) handleDelete(ctx context.Context, ev *contentevent.ContentEvent) error {
	if c.deleter == nil {
		return nil
	}
	at := ev.OccurredAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	n, err := c.deleter.InvalidateByDocument(ctx, ev.TenantID, ev.DocumentID, at, c.cfg.PurgeOnDelete)
	if err != nil {
		return err
	}
	if n > 0 {
		c.log.Info("invalidated memories for deleted document",
			"document_id", ev.DocumentID, "memories", n, "purged", c.cfg.PurgeOnDelete)
	}
	return nil
}

// Stop drains in-flight work and closes the connection.
func (c *Consumer) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cc != nil {
		c.cc.Drain()
		c.cc = nil
	}
	if c.nc != nil {
		_ = c.nc.Drain()
		c.nc = nil
	}
}

// Healthy reports connection state for /readyz.
func (c *Consumer) Healthy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nc != nil && c.nc.IsConnected()
}

// compile-time check that the store satisfies Deleter.
var _ Deleter = (*store.Store)(nil)
