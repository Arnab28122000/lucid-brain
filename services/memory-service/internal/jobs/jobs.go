// Package jobs holds the background maintenance that keeps the memory layer
// from degrading into an append-only log: decay, which lets unused memories
// fade, and consolidation, which folds accumulated fragments into one statement.
//
// Both run per tenant on a timer rather than on the ingest path. Neither is on
// any latency budget, and doing this work inline would make ingestion slower in
// exactly the moment — a backfill spike — when it is already the bottleneck.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cortex-ai/cortex/services/memory-service/internal/llm"
	"github.com/cortex-ai/cortex/services/memory-service/internal/memory"
	"github.com/cortex-ai/cortex/services/memory-service/internal/store"
)

// Runner executes the maintenance jobs on a schedule.
type Runner struct {
	Store *store.Store
	LLM   llm.Client
	Log   *slog.Logger

	DecayInterval         time.Duration
	ConsolidateInterval   time.Duration
	SalienceFloor         float64
	ConsolidationMinGroup int
	Now                   func() time.Time
}

// New builds a runner with defaults.
func New(st *store.Store, client llm.Client, log *slog.Logger) *Runner {
	return &Runner{
		Store:                 st,
		LLM:                   client,
		Log:                   log,
		DecayInterval:         6 * time.Hour,
		ConsolidateInterval:   24 * time.Hour,
		SalienceFloor:         0.15,
		ConsolidationMinGroup: 6,
		Now:                   func() time.Time { return time.Now().UTC() },
	}
}

// Run blocks until ctx is cancelled, ticking both jobs.
func (r *Runner) Run(ctx context.Context) {
	decay := time.NewTicker(r.DecayInterval)
	consolidate := time.NewTicker(r.ConsolidateInterval)
	defer decay.Stop()
	defer consolidate.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-decay.C:
			if err := r.RunDecay(ctx); err != nil {
				r.Log.Error("decay job failed", "error", err)
			}
		case <-consolidate.C:
			if err := r.RunConsolidation(ctx); err != nil {
				r.Log.Error("consolidation job failed", "error", err)
			}
		}
	}
}

// RunDecay applies exponential decay across every tenant.
func (r *Runner) RunDecay(ctx context.Context) error {
	tenants, err := r.Store.Tenants(ctx)
	if err != nil {
		return err
	}
	now := r.Now()
	for _, tenant := range tenants {
		res, err := r.Store.ApplyDecay(ctx, tenant, now, r.SalienceFloor)
		if err != nil {
			// One tenant's failure must not stop the sweep; a shared-shard
			// deployment would otherwise let the first bad tenant starve
			// everyone alphabetically after it.
			r.Log.Error("decay failed for tenant", "tenant_id", tenant, "error", err)
			continue
		}
		r.Log.Info("decay applied",
			"tenant_id", tenant, "scanned", res.Scanned, "decayed", res.Decayed, "archived", res.Archived)
	}
	return nil
}

const consolidateSystem = `You merge several related memories about one topic into a single accurate statement.

Return JSON: {"statement": "...", "topic": "...", "confidence": 0.0-1.0, "attributes": {"key":"value"}}

Rules:
- The statement must be supported by the inputs and must not add anything they do not say.
- Preserve every specific: names, numbers, dates, versions. Losing a specific is worse than being verbose.
- If the inputs disagree, say so explicitly rather than picking one.
- The statement must be self-contained, with no pronouns and no reference to "these memories".
- Keep it under 300 characters if the facts allow it.`

type consolidateResponse struct {
	Statement  string            `json:"statement"`
	Topic      string            `json:"topic"`
	Confidence float64           `json:"confidence"`
	Attributes map[string]string `json:"attributes"`
}

// RunConsolidation folds over-accumulated topics into summary memories.
func (r *Runner) RunConsolidation(ctx context.Context) error {
	tenants, err := r.Store.Tenants(ctx)
	if err != nil {
		return err
	}
	for _, tenant := range tenants {
		groups, err := r.Store.FindConsolidationGroups(ctx, tenant, r.ConsolidationMinGroup, 20)
		if err != nil {
			r.Log.Error("consolidation scan failed", "tenant_id", tenant, "error", err)
			continue
		}
		for _, g := range groups {
			if err := r.consolidateGroup(ctx, g); err != nil {
				r.Log.Error("consolidation failed",
					"tenant_id", tenant, "topic", g.Topic, "kind", g.Kind, "error", err)
			}
		}
	}
	return nil
}

func (r *Runner) consolidateGroup(ctx context.Context, g store.ConsolidationGroup) error {
	raw, err := r.LLM.Complete(ctx, llm.Request{
		System:      consolidateSystem,
		User:        renderGroup(g),
		MaxTokens:   1024,
		Temperature: 0,
		CacheKey:    "memory-consolidate-v1",
	})
	if err != nil {
		return err
	}
	jsonText, err := llm.ExtractJSON(raw)
	if err != nil {
		return err
	}
	var resp consolidateResponse
	if err := json.Unmarshal([]byte(jsonText), &resp); err != nil {
		return fmt.Errorf("consolidate: decode: %w", err)
	}
	statement := strings.TrimSpace(resp.Statement)
	if statement == "" {
		return fmt.Errorf("consolidate: empty statement for topic %q", g.Topic)
	}

	topicRaw := strings.TrimSpace(resp.Topic)
	if topicRaw == "" {
		topicRaw = g.Memories[0].TopicRaw
	}
	confidence := resp.Confidence
	if confidence <= 0 || confidence > 1 {
		// A summary is never more certain than its weakest input, so fall back
		// to the minimum rather than to an optimistic default.
		confidence = 1
		for _, m := range g.Memories {
			if m.Confidence < confidence {
				confidence = m.Confidence
			}
		}
	}

	id, err := r.Store.Consolidate(ctx, g, memory.Memory{
		Statement:  statement,
		TopicRaw:   topicRaw,
		Confidence: confidence,
		Attributes: resp.Attributes,
		IngestedAt: r.Now(),
	})
	if err != nil {
		return err
	}
	r.Log.Info("memories consolidated",
		"tenant_id", g.TenantID, "topic", g.Topic, "kind", g.Kind,
		"merged", len(g.Memories), "memory_id", id)
	return nil
}

func renderGroup(g store.ConsolidationGroup) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Topic: %s\nKind: %s\n\nMemories (oldest first):\n", g.Topic, g.Kind)
	for i := len(g.Memories) - 1; i >= 0; i-- {
		m := g.Memories[i]
		fmt.Fprintf(&b, "- [%s] %s", m.ValidFrom.Format("2006-01-02"), m.Statement)
		if len(m.Attributes) > 0 {
			attrs := make([]string, 0, len(m.Attributes))
			for k, v := range m.Attributes {
				attrs = append(attrs, k+"="+v)
			}
			// Sorted so the prompt is stable across runs and the gateway's
			// prompt cache can actually hit.
			sortStrings(attrs)
			fmt.Fprintf(&b, " (%s)", strings.Join(attrs, ", "))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func sortStrings(in []string) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}
