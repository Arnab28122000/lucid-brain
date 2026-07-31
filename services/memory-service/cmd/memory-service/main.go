// Command memory-service runs the Cortex temporal memory layer: a JetStream
// consumer that extracts memories from content events, the bi-temporal store
// behind them, the maintenance jobs that keep the store from growing stale, and
// the read API the query gateway and decisions timeline call.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cortex-ai/cortex/services/memory-service/internal/api"
	"github.com/cortex-ai/cortex/services/memory-service/internal/bus"
	"github.com/cortex-ai/cortex/services/memory-service/internal/config"
	"github.com/cortex-ai/cortex/services/memory-service/internal/extract"
	"github.com/cortex-ai/cortex/services/memory-service/internal/jobs"
	"github.com/cortex-ai/cortex/services/memory-service/internal/llm"
	"github.com/cortex-ai/cortex/services/memory-service/internal/pipeline"
	"github.com/cortex-ai/cortex/services/memory-service/internal/store"
	"github.com/cortex-ai/cortex/services/memory-service/migrations"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)

	// Signals cancel this context, which unwinds the consumer, the jobs and
	// the HTTP server in that order.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, store.Options{
		DSN:          cfg.PostgresDSN,
		GraphEnabled: cfg.GraphEnabled,
		MaxConns:     cfg.MaxConns,
	})
	if err != nil {
		return err
	}
	defer st.Close()
	log.Info("connected to postgres", "graph_enabled", cfg.GraphEnabled)

	if cfg.AutoMigrate {
		if err := applyMigrations(ctx, st, log); err != nil {
			return err
		}
	}

	llmClient := llm.NewHTTP(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
	pipe := pipeline.New(st, extract.New(llmClient), log)

	var consumer *bus.Consumer
	if !cfg.ConsumerOff {
		consumer, err = bus.NewConsumer(ctx, bus.Config{
			URL:           cfg.NATSURL,
			Stream:        cfg.NATSStream,
			Subject:       cfg.NATSSubject,
			Durable:       cfg.NATSDurable,
			MaxInflight:   cfg.MaxInflight,
			AckWait:       cfg.AckWait,
			MaxDeliver:    cfg.MaxDeliver,
			PurgeOnDelete: cfg.PurgeOnDelete,
		}, pipe, st, log)
		if err != nil {
			return err
		}
		if err := consumer.Start(ctx); err != nil {
			return err
		}
		defer consumer.Stop()
	} else {
		log.Warn("event consumer disabled; episodes accepted only via POST /v1/episodes")
	}

	if cfg.JobsEnabled {
		runner := jobs.New(st, llmClient, log)
		runner.DecayInterval = cfg.DecayInterval
		runner.ConsolidateInterval = cfg.ConsolidateInterval
		runner.SalienceFloor = cfg.SalienceFloor
		go runner.Run(ctx)
		log.Info("maintenance jobs scheduled",
			"decay_interval", cfg.DecayInterval, "consolidate_interval", cfg.ConsolidateInterval)
	}

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: (&api.Server{
			Store:    st,
			Pipeline: pipe,
			Log:      log,
			Ready: func() bool {
				return consumer == nil || consumer.Healthy()
			},
		}).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// Generous, because POST /v1/episodes runs a full extraction inline for
		// the eval harness and for single-document replay during an incident.
		WriteTimeout: 3 * time.Minute,
		IdleTimeout:  60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("memory-service listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	return nil
}

// applyMigrations is for the docker-compose evaluation mode. In production a
// Helm pre-install job applies the same embedded SQL, so the schema is never
// applied by N racing replicas.
func applyMigrations(ctx context.Context, st *store.Store, log *slog.Logger) error {
	all, err := migrations.All()
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	for _, m := range all {
		if _, err := st.Pool().Exec(ctx, m.SQL); err != nil {
			return fmt.Errorf("apply migration %s: %w", m.Name, err)
		}
		log.Info("migration applied", "name", m.Name)
	}
	return nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
