// Package extract turns an episode into candidate memories using two LLM passes
// with different jobs.
//
// The broad pass reads a large window and asks "what would someone need to know
// from this?" — it is good at gist and terrible at specifics; it will happily
// write "pricing was raised" and drop the number. The detail pass reads the same
// window and asks only for the specifics — names, prices, versions, dates,
// identifiers — with the exact source span each was taken from. Neither pass
// alone is sufficient: one pass tuned for both objectives reliably does one of
// them badly.
//
// The two candidate sets are then merged on the normalized topic, with detail
// attributes grafted onto broad statements. What survives goes to the verifier.
package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cortex-ai/cortex/services/memory-service/internal/llm"
	"github.com/cortex-ai/cortex/services/memory-service/internal/memory"
)

// Pass identifies which extraction pass produced a candidate; the verifier
// applies stricter attribute checks to detail-pass output.
type Pass string

const (
	PassBroad  Pass = "broad"
	PassDetail Pass = "detail"
	PassMerged Pass = "merged"
)

// Candidate is a proposed memory before verification. It is deliberately not a
// memory.Memory — nothing that has not passed the verifier should be
// constructible as one.
type Candidate struct {
	Kind       memory.Kind
	TopicRaw   string
	Topic      string
	Statement  string
	Attributes map[string]string
	Subjects   []string
	TaskStatus memory.TaskStatus
	Confidence float64
	// Quote is the source span the candidate was drawn from — the grounding
	// check runs against it, and it is shown in citations.
	Quote      string
	Pass       Pass
	ChunkIndex int
	// ValidFrom defaults to the episode's occurrence time but the extractor may
	// override it when the text states when something became true.
	ValidFrom time.Time
}

// Extractor runs the two passes over an episode.
type Extractor struct {
	LLM llm.Client
	// MaxChunks bounds LLM spend on pathologically long episodes (a year of a
	// busy channel exported as one document). Truncation is logged by the
	// caller, never silent.
	MaxChunks int
}

// New builds an extractor with defaults sized for the backfill smoothing budget.
func New(client llm.Client) *Extractor {
	return &Extractor{LLM: client, MaxChunks: 12}
}

const broadSystem = `You extract durable memories from enterprise content for a company knowledge system.

Return JSON: {"memories":[{"kind","topic","statement","subjects","task_status","confidence","quote"}]}

kind is one of: fact, event, instruction, task, decision.
- fact: something that is true and stays true until changed
- event: something that happened at a point in time
- instruction: a standing rule, policy or preference to follow
- task: an action item someone owes
- decision: a choice that was made, ideally with what it ruled out

Rules:
- statement must be self-contained: no "he", "it", "this", "the above", "yesterday". Name the people, systems and dates explicitly.
- topic is a short noun phrase naming what the memory is ABOUT (3-6 words), not a summary of it.
- quote must be copied verbatim from the source, and must be the span that supports the statement.
- subjects lists the people the memory is about, by name as written in the source.
- task_status is "open", "done" or "dropped", and only for kind=task.
- confidence in [0,1]: how certain you are this is asserted by the source rather than inferred.
- Extract nothing rather than something weak. Small talk, greetings and scheduling chatter are not memories.
- Return at most 12 memories.`

const detailSystem = `You extract SPECIFICS from enterprise content. Not summaries — specifics.

Return JSON: {"details":[{"topic","statement","attributes":{"key":"value"},"quote","confidence","kind"}]}

Extract only statements that carry at least one concrete particular:
- named entities (people, teams, systems, customers, vendors)
- numbers: prices, quantities, limits, percentages, dates, durations
- versions, release tags, environment names, region codes, identifiers, error codes

Rules:
- Every value in attributes MUST appear verbatim in the source text. Copy, never compute, never round, never reformat. If the source says "$1.02/M tokens" the value is "$1.02/M tokens".
- quote must be copied verbatim and must contain the attribute values.
- statement is one sentence stating the particular, self-contained, with no pronouns.
- topic is a short noun phrase naming what it is about.
- kind is one of: fact, event, instruction, task, decision.
- Return nothing if the text carries no specifics. An empty list is a correct answer.
- Return at most 12 details.`

type broadResponse struct {
	Memories []struct {
		Kind       string   `json:"kind"`
		Topic      string   `json:"topic"`
		Statement  string   `json:"statement"`
		Subjects   []string `json:"subjects"`
		TaskStatus string   `json:"task_status"`
		Confidence float64  `json:"confidence"`
		Quote      string   `json:"quote"`
	} `json:"memories"`
}

type detailResponse struct {
	Details []struct {
		Kind       string            `json:"kind"`
		Topic      string            `json:"topic"`
		Statement  string            `json:"statement"`
		Attributes map[string]string `json:"attributes"`
		Confidence float64           `json:"confidence"`
		Quote      string            `json:"quote"`
	} `json:"details"`
}

// Extract runs both passes over every chunk and returns merged candidates.
//
// The passes are independent per chunk, so a malformed response from one pass
// on one chunk costs that chunk's contribution and nothing else. Losing a chunk
// is recoverable — the episode can be reprocessed; failing the whole episode
// because the model emitted a stray token is not a trade worth making.
func (e *Extractor) Extract(ctx context.Context, ep memory.Episode) ([]Candidate, []error) {
	chunks := ChunkBody(ep.Body)
	if e.MaxChunks > 0 && len(chunks) > e.MaxChunks {
		chunks = chunks[:e.MaxChunks]
	}

	var (
		broad  []Candidate
		detail []Candidate
		errs   []error
	)
	for _, ch := range chunks {
		b, err := e.broadPass(ctx, ep, ch)
		if err != nil {
			errs = append(errs, fmt.Errorf("broad pass chunk %d: %w", ch.Index, err))
		}
		broad = append(broad, b...)

		d, err := e.detailPass(ctx, ep, ch)
		if err != nil {
			errs = append(errs, fmt.Errorf("detail pass chunk %d: %w", ch.Index, err))
		}
		detail = append(detail, d...)
	}
	return Merge(broad, detail), errs
}

func (e *Extractor) broadPass(ctx context.Context, ep memory.Episode, ch Chunk) ([]Candidate, error) {
	raw, err := e.LLM.Complete(ctx, llm.Request{
		System:      broadSystem,
		User:        renderPrompt(ep, ch),
		MaxTokens:   2048,
		Temperature: 0,
		CacheKey:    "memory-broad-v1",
	})
	if err != nil {
		return nil, err
	}
	jsonText, err := llm.ExtractJSON(raw)
	if err != nil {
		return nil, err
	}
	var resp broadResponse
	if err := json.Unmarshal([]byte(jsonText), &resp); err != nil {
		return nil, fmt.Errorf("broad: decode: %w", err)
	}

	out := make([]Candidate, 0, len(resp.Memories))
	for _, m := range resp.Memories {
		kind, err := memory.ParseKind(m.Kind)
		if err != nil {
			// An unclassifiable candidate is dropped, not defaulted to fact:
			// defaulting would push events into the supersession path where
			// they would invalidate real facts on the same topic.
			continue
		}
		c := Candidate{
			Kind:       kind,
			TopicRaw:   strings.TrimSpace(m.Topic),
			Topic:      memory.NormalizeTopic(m.Topic),
			Statement:  strings.TrimSpace(m.Statement),
			Subjects:   normalizeSubjects(m.Subjects, ep),
			Confidence: clamp01(m.Confidence),
			Quote:      strings.TrimSpace(m.Quote),
			Pass:       PassBroad,
			ChunkIndex: ch.Index,
			ValidFrom:  ep.OccurredAt,
		}
		if kind == memory.KindTask {
			c.TaskStatus = parseTaskStatus(m.TaskStatus)
		}
		out = append(out, c)
	}
	return out, nil
}

func (e *Extractor) detailPass(ctx context.Context, ep memory.Episode, ch Chunk) ([]Candidate, error) {
	raw, err := e.LLM.Complete(ctx, llm.Request{
		System:      detailSystem,
		User:        renderPrompt(ep, ch),
		MaxTokens:   2048,
		Temperature: 0,
		CacheKey:    "memory-detail-v1",
	})
	if err != nil {
		return nil, err
	}
	jsonText, err := llm.ExtractJSON(raw)
	if err != nil {
		return nil, err
	}
	var resp detailResponse
	if err := json.Unmarshal([]byte(jsonText), &resp); err != nil {
		return nil, fmt.Errorf("detail: decode: %w", err)
	}

	out := make([]Candidate, 0, len(resp.Details))
	for _, d := range resp.Details {
		kind, err := memory.ParseKind(d.Kind)
		if err != nil {
			kind = memory.KindFact // a specific with no stated kind is a fact about the world
		}
		attrs := make(map[string]string, len(d.Attributes))
		for k, v := range d.Attributes {
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			if k == "" || v == "" {
				continue
			}
			attrs[k] = v
		}
		if len(attrs) == 0 {
			// The detail pass exists to produce attributes. Without any, this
			// is just a worse broad-pass candidate.
			continue
		}
		out = append(out, Candidate{
			Kind:       kind,
			TopicRaw:   strings.TrimSpace(d.Topic),
			Topic:      memory.NormalizeTopic(d.Topic),
			Statement:  strings.TrimSpace(d.Statement),
			Attributes: attrs,
			Confidence: clamp01(d.Confidence),
			Quote:      strings.TrimSpace(d.Quote),
			Pass:       PassDetail,
			ChunkIndex: ch.Index,
			ValidFrom:  ep.OccurredAt,
		})
	}
	return out, nil
}

// Merge folds detail candidates into broad ones on the same topic and kind,
// attaching the specifics to the better-formed statement. Detail candidates on
// topics the broad pass missed are kept standalone — a price change nobody
// summarized is still a price change.
func Merge(broad, detail []Candidate) []Candidate {
	merged := make([]Candidate, 0, len(broad)+len(detail))
	merged = append(merged, dedupe(broad)...)

	for _, d := range dedupe(detail) {
		var target *Candidate
		for i := range merged {
			if merged[i].Kind == d.Kind && merged[i].Topic == d.Topic {
				target = &merged[i]
				break
			}
		}
		if target == nil {
			merged = append(merged, d)
			continue
		}
		if target.Attributes == nil {
			target.Attributes = make(map[string]string, len(d.Attributes))
		}
		for k, v := range d.Attributes {
			if _, exists := target.Attributes[k]; !exists {
				target.Attributes[k] = v
			}
		}
		// Two passes independently landing on the same topic is genuine
		// corroboration, so take the higher confidence rather than averaging.
		if d.Confidence > target.Confidence {
			target.Confidence = d.Confidence
		}
		// The detail quote contains the particulars, which is what the
		// attribute-fidelity check needs to verify against.
		if len(d.Quote) > len(target.Quote) {
			target.Quote = d.Quote
		}
		target.Pass = PassMerged
	}

	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].Kind != merged[j].Kind {
			return merged[i].Kind < merged[j].Kind
		}
		return merged[i].Topic < merged[j].Topic
	})
	return merged
}

// dedupe collapses candidates repeated across overlapping chunks. The overlap
// window exists so straddling content is seen twice; the cost is that it is also
// *extracted* twice.
func dedupe(in []Candidate) []Candidate {
	out := make([]Candidate, 0, len(in))
	for _, c := range in {
		dup := false
		for i := range out {
			if out[i].Kind == c.Kind && out[i].Topic == c.Topic && similarStatement(out[i].Statement, c.Statement) {
				if c.Confidence > out[i].Confidence {
					out[i].Confidence = c.Confidence
				}
				for k, v := range c.Attributes {
					if out[i].Attributes == nil {
						out[i].Attributes = map[string]string{}
					}
					if _, exists := out[i].Attributes[k]; !exists {
						out[i].Attributes[k] = v
					}
				}
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, c)
		}
	}
	return out
}

// similarStatement is a token-overlap test, not a semantic one. It exists to
// catch the same sentence re-extracted from an overlapping window, where the
// wording is identical or near-identical; anything subtler is the verifier's and
// the consolidation job's problem.
func similarStatement(a, b string) bool {
	if strings.EqualFold(a, b) {
		return true
	}
	// Stricter than the store's reinforcement threshold: this only has to catch
	// the same sentence seen twice through the chunk overlap window, where the
	// wording is identical. Real near-duplicates are the verifier's job.
	return memory.StatementSimilarity(a, b) >= 0.85
}

func renderPrompt(ep memory.Episode, ch Chunk) string {
	var b strings.Builder
	b.WriteString("Source: ")
	b.WriteString(ep.SourceType)
	if ep.Title != "" {
		b.WriteString("\nTitle: ")
		b.WriteString(ep.Title)
	}
	if ep.Author.DisplayName != "" {
		b.WriteString("\nAuthor: ")
		b.WriteString(ep.Author.DisplayName)
	}
	if len(ep.Participants) > 0 {
		names := make([]string, 0, len(ep.Participants))
		for _, p := range ep.Participants {
			if p.DisplayName != "" {
				names = append(names, p.DisplayName)
			}
		}
		if len(names) > 0 {
			b.WriteString("\nParticipants: ")
			b.WriteString(strings.Join(names, ", "))
		}
	}
	// The date is given explicitly so the model can resolve "yesterday" and
	// "next sprint" into absolute dates in the statement it writes.
	b.WriteString("\nDate: ")
	b.WriteString(ep.OccurredAt.UTC().Format(time.RFC3339))
	b.WriteString("\n\n---\n")
	b.WriteString(ch.Text)
	b.WriteString("\n---\n")
	return b.String()
}

func normalizeSubjects(raw []string, ep memory.Episode) []string {
	if len(raw) == 0 {
		return nil
	}
	// Resolve names the extractor wrote back onto participant identities where
	// possible, so expertise edges key on stable external IDs rather than on
	// however someone's display name was spelled that day.
	people := append([]memory.Person{ep.Author}, ep.Participants...)
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, name := range raw {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		key := "name:" + strings.ToLower(name)
		for _, p := range people {
			if p.DisplayName != "" && strings.EqualFold(p.DisplayName, name) {
				key = p.Key()
				break
			}
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}

func parseTaskStatus(s string) memory.TaskStatus {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "done", "closed", "complete", "completed":
		return memory.TaskDone
	case "dropped", "cancelled", "canceled", "abandoned":
		return memory.TaskDropped
	default:
		return memory.TaskOpen
	}
}

func clamp01(f float64) float64 {
	switch {
	case f <= 0:
		// A model that omits confidence should not thereby get the highest
		// confidence, nor the lowest — 0.5 is the honest default.
		return 0.5
	case f > 1:
		return 1
	default:
		return f
	}
}
