// Package pipeline wires extraction, verification and the bi-temporal store
// into the single operation the consumer and the API both call: turn one
// episode into committed memories.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cortex-ai/cortex/services/memory-service/internal/extract"
	"github.com/cortex-ai/cortex/services/memory-service/internal/memory"
	"github.com/cortex-ai/cortex/services/memory-service/internal/store"
	"github.com/cortex-ai/cortex/services/memory-service/internal/verify"
)

// Pipeline processes episodes.
type Pipeline struct {
	Store     *store.Store
	Extractor *extract.Extractor
	Verifier  *verify.Verifier
	Log       *slog.Logger
	Now       func() time.Time
}

// New builds a pipeline with production defaults.
func New(st *store.Store, ex *extract.Extractor, log *slog.Logger) *Pipeline {
	return &Pipeline{
		Store:     st,
		Extractor: ex,
		Verifier:  verify.New(),
		Log:       log,
		Now:       func() time.Time { return time.Now().UTC() },
	}
}

// Report is the outcome of processing one episode.
type Report struct {
	EpisodeID  string               `json:"episode_id"`
	Skipped    bool                 `json:"skipped"`
	Reason     string               `json:"reason,omitempty"`
	Candidates int                  `json:"candidates"`
	Accepted   int                  `json:"accepted"`
	Rejected   int                  `json:"rejected"`
	Outcomes   map[string]int       `json:"outcomes,omitempty"`
	Superseded []store.Supersession `json:"superseded,omitempty"`
	MemoryIDs  []string             `json:"memory_ids,omitempty"`
	// PartialErrors are per-chunk extraction failures the episode survived.
	// Surfaced rather than swallowed: a run that quietly lost half its chunks
	// looks identical to a thin document otherwise.
	PartialErrors []string `json:"partial_errors,omitempty"`
}

// Process runs one episode end to end.
//
// Ordering is deliberate: the idempotency check comes before the LLM call, and
// the LLM call comes before anything is written. An episode that fails
// mid-extraction leaves no trace and will be reprocessed on redelivery.
func (p *Pipeline) Process(ctx context.Context, ep memory.Episode) (Report, error) {
	rep := Report{EpisodeID: ep.ID}

	done, err := p.Store.AlreadyProcessed(ctx, ep.TenantID, ep.ContentHash)
	if err != nil {
		return rep, err
	}
	if done {
		rep.Skipped, rep.Reason = true, "already processed"
		return rep, nil
	}

	candidates, extractErrs := p.Extractor.Extract(ctx, ep)
	for _, e := range extractErrs {
		rep.PartialErrors = append(rep.PartialErrors, e.Error())
	}
	// Every chunk failing is not a thin document, it is a broken dependency —
	// returning success would mark the episode processed and lose it for good.
	if len(candidates) == 0 && len(extractErrs) > 0 {
		return rep, fmt.Errorf("extraction produced nothing: %w", errors.Join(extractErrs...))
	}
	rep.Candidates = len(candidates)

	accepted, rejections := p.Verifier.VerifyAll(candidates, ep)
	rep.Accepted, rep.Rejected = len(accepted), len(rejections)

	proposals := make([]store.Proposal, 0, len(accepted))
	now := p.Now()
	for _, c := range accepted {
		proposals = append(proposals, store.Proposal{
			Memory: memory.Memory{
				TenantID:    ep.TenantID,
				Kind:        c.Kind,
				Topic:       c.Topic,
				TopicRaw:    c.TopicRaw,
				Statement:   c.Statement,
				Attributes:  c.Attributes,
				Subjects:    c.Subjects,
				TaskStatus:  c.TaskStatus,
				Confidence:  c.Confidence,
				Salience:    1,
				ValidFrom:   c.ValidFrom,
				IngestedAt:  now,
				ACLGroupIDs: ep.ACLGroupIDs,
				LastSeenAt:  ep.OccurredAt,
			},
			Quote: c.Quote,
			Pass:  string(c.Pass),
		})
	}

	records := make([]store.RejectionRecord, 0, len(rejections))
	for _, r := range rejections {
		records = append(records, store.RejectionRecord{
			Kind:        string(r.Candidate.Kind),
			Topic:       r.Candidate.Topic,
			Statement:   r.Candidate.Statement,
			FailedCheck: r.Verdict.FailedCheck(),
			Reason:      r.Verdict.Reason(),
			Checks:      r.Verdict.Checks,
		})
	}

	result, err := p.Store.Commit(ctx, ep, proposals, records)
	if err != nil {
		return rep, err
	}

	rep.MemoryIDs = result.MemoryIDs
	rep.Superseded = result.Superseded
	rep.Outcomes = map[string]int{}
	for k, v := range result.Outcomes {
		rep.Outcomes[string(k)] = v
	}

	if p.Log != nil {
		p.Log.Info("episode processed",
			"episode_id", ep.ID,
			"tenant_id", ep.TenantID,
			"source", ep.SourceType,
			"candidates", rep.Candidates,
			"accepted", rep.Accepted,
			"rejected", rep.Rejected,
			"superseded", len(rep.Superseded),
			"partial_errors", len(rep.PartialErrors))
		for _, r := range rejections {
			p.Log.Debug("candidate rejected",
				"episode_id", ep.ID,
				"check", r.Verdict.FailedCheck(),
				"reason", r.Verdict.Reason(),
				"statement", r.Candidate.Statement)
		}
	}
	return rep, nil
}
