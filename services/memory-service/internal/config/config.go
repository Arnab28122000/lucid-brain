// Package config loads the service's environment configuration.
//
// Environment variables only: the service runs under Kubernetes with values
// from ConfigMaps and Secrets, and a config file would just be a second place
// for the same values to disagree.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the full service configuration.
type Config struct {
	Addr     string
	LogLevel string

	PostgresDSN  string
	GraphEnabled bool
	MaxConns     int32
	AutoMigrate  bool

	NATSURL       string
	NATSStream    string
	NATSSubject   string
	NATSDurable   string
	MaxInflight   int
	AckWait       time.Duration
	MaxDeliver    int
	PurgeOnDelete bool
	ConsumerOff   bool

	LLMBaseURL string
	LLMAPIKey  string
	LLMModel   string

	JobsEnabled         bool
	DecayInterval       time.Duration
	ConsolidateInterval time.Duration
	SalienceFloor       float64
}

// Load reads configuration from the environment, applying defaults.
func Load() (Config, error) {
	c := Config{
		Addr:                env("MEMORY_ADDR", ":8087"),
		LogLevel:            env("MEMORY_LOG_LEVEL", "info"),
		PostgresDSN:         os.Getenv("MEMORY_POSTGRES_DSN"),
		GraphEnabled:        envBool("MEMORY_GRAPH_ENABLED", false),
		MaxConns:            int32(envInt("MEMORY_PG_MAX_CONNS", 10)),
		AutoMigrate:         envBool("MEMORY_AUTO_MIGRATE", false),
		NATSURL:             env("MEMORY_NATS_URL", "nats://localhost:4222"),
		NATSStream:          env("MEMORY_NATS_STREAM", ""),
		NATSSubject:         env("MEMORY_NATS_SUBJECT", ""),
		NATSDurable:         env("MEMORY_NATS_DURABLE", "memory-service"),
		MaxInflight:         envInt("MEMORY_MAX_INFLIGHT", 8),
		AckWait:             envDuration("MEMORY_ACK_WAIT", 5*time.Minute),
		MaxDeliver:          envInt("MEMORY_MAX_DELIVER", 5),
		PurgeOnDelete:       envBool("MEMORY_PURGE_ON_DELETE", false),
		ConsumerOff:         envBool("MEMORY_CONSUMER_DISABLED", false),
		LLMBaseURL:          env("MEMORY_LLM_BASE_URL", "http://llm-gateway:8080"),
		LLMAPIKey:           os.Getenv("MEMORY_LLM_API_KEY"),
		LLMModel:            env("MEMORY_LLM_MODEL", "cortex-extract"),
		JobsEnabled:         envBool("MEMORY_JOBS_ENABLED", true),
		DecayInterval:       envDuration("MEMORY_DECAY_INTERVAL", 6*time.Hour),
		ConsolidateInterval: envDuration("MEMORY_CONSOLIDATE_INTERVAL", 24*time.Hour),
		SalienceFloor:       envFloat("MEMORY_SALIENCE_FLOOR", 0.15),
	}

	if c.PostgresDSN == "" {
		return c, errors.New("config: MEMORY_POSTGRES_DSN is required")
	}
	if c.AckWait <= time.Minute {
		// The consumer derives a per-message deadline of AckWait-30s, so a
		// short AckWait would produce a nonsensical or negative timeout.
		return c, fmt.Errorf("config: MEMORY_ACK_WAIT must exceed 1m, got %s", c.AckWait)
	}
	return c, nil
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envFloat(key string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func envDuration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
