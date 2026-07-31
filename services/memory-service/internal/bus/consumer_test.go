package bus_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/cortex-ai/cortex/services/memory-service/internal/bus"
	"github.com/cortex-ai/cortex/services/memory-service/internal/contentevent"
	"github.com/cortex-ai/cortex/services/memory-service/internal/memory"
	"github.com/cortex-ai/cortex/services/memory-service/internal/pipeline"
)

// Consumer behaviour is mostly JetStream semantics — ack versus nak versus
// term, redelivery, durable binding — so it is tested against a real server.
//
//	docker run -d -p 4223:4222 nats:latest -js
//	MEMORY_TEST_NATS='nats://localhost:4223' go test ./internal/bus/...

type stubHandler struct {
	mu       sync.Mutex
	episodes []memory.Episode
	err      error
	calls    int
	done     chan struct{}
}

func (h *stubHandler) Process(_ context.Context, ep memory.Episode) (pipeline.Report, error) {
	h.mu.Lock()
	h.calls++
	if h.err == nil {
		h.episodes = append(h.episodes, ep)
	}
	err := h.err
	h.mu.Unlock()

	select {
	case h.done <- struct{}{}:
	default:
	}
	return pipeline.Report{EpisodeID: ep.ID}, err
}

func (h *stubHandler) snapshot() (int, []memory.Episode) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls, append([]memory.Episode(nil), h.episodes...)
}

type stubDeleter struct {
	mu       sync.Mutex
	deleted  []string
	notified chan struct{}
}

func (d *stubDeleter) InvalidateByDocument(_ context.Context, _, documentID string, _ time.Time, _ bool) (int, error) {
	d.mu.Lock()
	d.deleted = append(d.deleted, documentID)
	d.mu.Unlock()
	select {
	case d.notified <- struct{}{}:
	default:
	}
	return 1, nil
}

func setup(t *testing.T) (jetstream.JetStream, string) {
	t.Helper()
	url := os.Getenv("MEMORY_TEST_NATS")
	if url == "" {
		t.Skip("MEMORY_TEST_NATS not set; skipping consumer integration tests")
	}

	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}

	// A stream per test keeps durable consumers and message state isolated.
	stream := fmt.Sprintf("TEST_CONTENT_%d", time.Now().UnixNano())
	ctx := context.Background()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     stream,
		Subjects: []string{"test." + stream + ".>"},
		Storage:  jetstream.MemoryStorage,
	}); err != nil {
		t.Fatalf("create stream: %v", err)
	}
	t.Cleanup(func() { _ = js.DeleteStream(context.Background(), stream) })

	return js, stream
}

func publish(t *testing.T, js jetstream.JetStream, stream string, ev contentevent.ContentEvent) {
	t.Helper()
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	subject := fmt.Sprintf("test.%s.%s.%s", stream, ev.TenantID, ev.SourceType)
	if _, err := js.Publish(context.Background(), subject, payload); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

func startConsumer(t *testing.T, stream string, h bus.Handler, d bus.Deleter) *bus.Consumer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c, err := bus.NewConsumer(ctx, bus.Config{
		URL:        os.Getenv("MEMORY_TEST_NATS"),
		Stream:     stream,
		Subject:    "test." + stream + ".>",
		Durable:    "memory-test",
		AckWait:    90 * time.Second,
		MaxDeliver: 2,
	}, h, d, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new consumer: %v", err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(c.Stop)
	return c
}

func waitFor(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func upsertEvent(tenant, hash string) contentevent.ContentEvent {
	return contentevent.ContentEvent{
		EventID:     "evt-" + hash,
		TenantID:    tenant,
		DocumentID:  "doc-" + hash,
		ContentHash: hash,
		Op:          contentevent.OpUpsert,
		SourceType:  "slack",
		Permalink:   "https://slack.example/" + hash,
		Body:        "Priya: we have decided to move the Enterprise plan to $40 per seat per month starting April.",
		Author:      contentevent.Person{ExternalID: "U-priya", DisplayName: "Priya"},
		ACLGroupIDs: []string{"g-eng"},
		OccurredAt:  time.Date(2026, 3, 12, 14, 30, 0, 0, time.UTC),
		IngestedAt:  time.Date(2026, 3, 12, 14, 31, 0, 0, time.UTC),
	}
}

func TestConsumerProcessesContentEvents(t *testing.T) {
	js, stream := setup(t)
	h := &stubHandler{done: make(chan struct{}, 4)}
	startConsumer(t, stream, h, nil)

	publish(t, js, stream, upsertEvent("acme", "h1"))
	waitFor(t, h.done, "the episode to be processed")

	_, episodes := h.snapshot()
	if len(episodes) != 1 {
		t.Fatalf("processed %d episodes, want 1", len(episodes))
	}
	ep := episodes[0]
	if ep.TenantID != "acme" || ep.ContentHash != "h1" {
		t.Errorf("episode = %+v", ep)
	}
	if len(ep.ACLGroupIDs) != 1 {
		t.Errorf("ACLs did not survive the wire: %v", ep.ACLGroupIDs)
	}
}

func TestConsumerSkipsThinBodiesWithoutRedelivery(t *testing.T) {
	js, stream := setup(t)
	h := &stubHandler{done: make(chan struct{}, 4)}
	startConsumer(t, stream, h, nil)

	thin := upsertEvent("acme", "thin")
	thin.Body = "👍"
	publish(t, js, stream, thin)

	// Then a real one, to establish that the consumer moved past the thin event
	// rather than being stuck redelivering it.
	publish(t, js, stream, upsertEvent("acme", "real"))
	waitFor(t, h.done, "the real episode to be processed")

	calls, episodes := h.snapshot()
	if calls != 1 || len(episodes) != 1 || episodes[0].ContentHash != "real" {
		t.Errorf("handler saw %d calls with episodes %+v; the reaction-only event should have been acked and dropped", calls, episodes)
	}
}

func TestConsumerRoutesDeletesToInvalidation(t *testing.T) {
	js, stream := setup(t)
	h := &stubHandler{done: make(chan struct{}, 4)}
	d := &stubDeleter{notified: make(chan struct{}, 2)}
	startConsumer(t, stream, h, d)

	del := upsertEvent("acme", "gone")
	del.Op = contentevent.OpDelete
	publish(t, js, stream, del)
	waitFor(t, d.notified, "the delete to be propagated")

	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.deleted) != 1 || d.deleted[0] != "doc-gone" {
		t.Errorf("deleted = %v, want the deleted document", d.deleted)
	}
	if calls, _ := h.snapshot(); calls != 0 {
		t.Error("a delete should take the invalidation path, not extraction")
	}
}

func TestConsumerTerminatesPoisonMessages(t *testing.T) {
	js, stream := setup(t)
	h := &stubHandler{done: make(chan struct{}, 4)}
	startConsumer(t, stream, h, nil)

	// Undecodable payload: it will never decode, so it must be terminated
	// rather than occupying a redelivery slot forever.
	if _, err := js.Publish(context.Background(), "test."+stream+".acme.slack", []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	publish(t, js, stream, upsertEvent("acme", "after-poison"))
	waitFor(t, h.done, "the following episode to be processed")

	_, episodes := h.snapshot()
	if len(episodes) != 1 || episodes[0].ContentHash != "after-poison" {
		t.Errorf("episodes = %+v; the poison message should not have blocked the stream", episodes)
	}
}

func TestConsumerRetriesFailedProcessing(t *testing.T) {
	js, stream := setup(t)
	h := &stubHandler{done: make(chan struct{}, 8), err: fmt.Errorf("llm gateway unavailable")}
	startConsumer(t, stream, h, nil)

	publish(t, js, stream, upsertEvent("acme", "retry"))
	waitFor(t, h.done, "the first delivery")
	// Backoff on the consumer's first step is 5s.
	waitFor(t, h.done, "the redelivery")

	if calls, _ := h.snapshot(); calls < 2 {
		t.Errorf("handler called %d times, want at least 2 — a transient failure must be retried, not acked", calls)
	}
}
