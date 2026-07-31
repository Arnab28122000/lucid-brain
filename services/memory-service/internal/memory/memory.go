// Package memory holds the domain model for the temporal memory layer: the
// memory record itself, its classification, and the bi-temporal fields that let
// the system answer "what is true now" as well as "what did we believe then".
package memory

import (
	"fmt"
	"time"
)

// Kind classifies an extracted memory. The taxonomy is deliberately small: every
// downstream behaviour (supersession, decay half-life, timeline eligibility)
// keys off it, so adding a kind means answering those three questions.
type Kind string

const (
	// KindFact is a durable statement about the world: "the EU region runs on
	// eu-central-1". Facts supersede on the same topic.
	KindFact Kind = "fact"
	// KindEvent is something that happened at a point in time. Events are
	// append-only — a new event never invalidates an older one.
	KindEvent Kind = "event"
	// KindInstruction is a standing directive: "always tag releases with the
	// sprint number". Instructions supersede on the same topic.
	KindInstruction Kind = "instruction"
	// KindTask is an actionable item with an owner and an open/closed lifecycle.
	KindTask Kind = "task"
	// KindDecision is a choice made by people, with the alternatives it ruled
	// out. Decisions supersede on the same topic and feed the decisions timeline.
	KindDecision Kind = "decision"
)

// AllKinds is the canonical ordering used by APIs and validation.
var AllKinds = []Kind{KindFact, KindEvent, KindInstruction, KindTask, KindDecision}

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool {
	for _, c := range AllKinds {
		if k == c {
			return true
		}
	}
	return false
}

// Supersedes reports whether a newer memory on the same normalized topic should
// invalidate the previous one. Events are a stream, not a state, so they never
// supersede; the other kinds describe current state and therefore do.
func (k Kind) Supersedes() bool { return k != KindEvent }

// HalfLife is how long an unreinforced memory of this kind takes to lose half
// its salience. Decisions and instructions are load-bearing long after they stop
// being mentioned; casual facts and closed tasks are not.
func (k Kind) HalfLife() time.Duration {
	switch k {
	case KindDecision:
		return 720 * 24 * time.Hour
	case KindInstruction:
		return 365 * 24 * time.Hour
	case KindFact:
		return 180 * 24 * time.Hour
	case KindTask:
		return 60 * 24 * time.Hour
	default: // events
		return 90 * 24 * time.Hour
	}
}

// ParseKind converts a raw LLM label into a Kind, tolerating the plurals and
// synonyms extraction models reach for.
func ParseKind(s string) (Kind, error) {
	switch normalizeLabel(s) {
	case "fact", "facts", "statement", "knowledge":
		return KindFact, nil
	case "event", "events", "incident", "occurrence":
		return KindEvent, nil
	case "instruction", "instructions", "directive", "policy", "rule", "preference":
		return KindInstruction, nil
	case "task", "tasks", "todo", "action", "actionitem", "action_item":
		return KindTask, nil
	case "decision", "decisions", "choice", "resolution":
		return KindDecision, nil
	}
	return "", fmt.Errorf("memory: unknown kind %q", s)
}

// TaskStatus tracks the lifecycle of KindTask memories.
type TaskStatus string

const (
	TaskOpen    TaskStatus = "open"
	TaskDone    TaskStatus = "done"
	TaskDropped TaskStatus = "dropped"
)

// Person is a participant reference carried through from the source system. The
// external ID is the source's stable identifier (Slack user ID, Atlassian
// account ID) and is what expertise edges are keyed on.
type Person struct {
	ExternalID  string `json:"external_id"`
	DisplayName string `json:"display_name,omitempty"`
	Email       string `json:"email,omitempty"`
}

// Key returns the identity used for people and expertise edges, preferring the
// stable external ID and falling back to email, then display name.
func (p Person) Key() string {
	switch {
	case p.ExternalID != "":
		return p.ExternalID
	case p.Email != "":
		return "email:" + normalizeLabel(p.Email)
	default:
		return "name:" + normalizeLabel(p.DisplayName)
	}
}

// Episode is the unit of source content a memory is extracted from: one Slack
// thread, one Confluence page revision, one meeting transcript. It is the anchor
// of the provenance index — every memory points back at the episodes that
// produced it, which is what makes a memory-backed answer citable.
type Episode struct {
	ID           string    `json:"id"`
	TenantID     string    `json:"tenant_id"`
	DocumentID   string    `json:"document_id"`
	ContentHash  string    `json:"content_hash"`
	SourceType   string    `json:"source_type"`
	SourceID     string    `json:"source_id"`
	Permalink    string    `json:"permalink"`
	Title        string    `json:"title"`
	Body         string    `json:"body"`
	Author       Person    `json:"author"`
	Participants []Person  `json:"participants,omitempty"`
	ACLGroupIDs  []string  `json:"acl_group_ids,omitempty"`
	OccurredAt   time.Time `json:"occurred_at"`
	IngestedAt   time.Time `json:"ingested_at"`
}

// Memory is a single extracted, verified assertion with bi-temporal validity.
//
// Two clocks matter and they are not the same clock. ValidFrom/ValidTo are
// *world* time: when the statement was true. IngestedAt is *system* time: when
// Cortex learned it. A Slack message from Tuesday ingested on Friday is valid
// from Tuesday but ingested Friday, and both are needed — the first to answer
// "what was true on Wednesday", the second to answer "what did we know on
// Wednesday", which is the question audits actually ask.
type Memory struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Kind     Kind   `json:"kind"`

	// Topic is the normalized key that supersession is computed over; TopicRaw
	// keeps the extractor's human-readable phrasing for display.
	Topic    string `json:"topic"`
	TopicRaw string `json:"topic_raw"`

	// Statement is a self-contained assertion, readable without its source.
	Statement string `json:"statement"`

	// Attributes carries the detail pass's structured findings — names, prices,
	// versions, dates — verbatim from the source so they can be checked.
	Attributes map[string]string `json:"attributes,omitempty"`

	Subjects   []string   `json:"subjects,omitempty"` // person keys this memory is about
	TaskStatus TaskStatus `json:"task_status,omitempty"`

	Confidence float64 `json:"confidence"`
	Salience   float64 `json:"salience"`

	ValidFrom  time.Time  `json:"valid_from"`
	ValidTo    *time.Time `json:"valid_to,omitempty"`
	IngestedAt time.Time  `json:"ingested_at"`

	// SupersededBy points at the memory that invalidated this one. Set together
	// with ValidTo; never accompanied by a delete.
	SupersededBy *string `json:"superseded_by,omitempty"`
	// Supersedes is the reverse pointer, populated on read for timelines.
	Supersedes []string `json:"supersedes,omitempty"`

	// EpisodeIDs is the provenance: the source episodes this was derived from.
	EpisodeIDs  []string `json:"episode_ids,omitempty"`
	ACLGroupIDs []string `json:"acl_group_ids,omitempty"`

	// LastSeenAt is the most recent reinforcement, used by the decay job.
	LastSeenAt time.Time `json:"last_seen_at"`
	Archived   bool      `json:"archived"`

	// Sources is populated on read so callers can render citations without a
	// second round trip.
	Sources []SourceRef `json:"sources,omitempty"`
}

// SourceRef is the citable half of the provenance index.
type SourceRef struct {
	EpisodeID  string    `json:"episode_id"`
	SourceType string    `json:"source_type"`
	Permalink  string    `json:"permalink"`
	Title      string    `json:"title"`
	OccurredAt time.Time `json:"occurred_at"`
	Quote      string    `json:"quote,omitempty"`
}

// CurrentAt reports whether the memory was valid in world time at t.
func (m *Memory) CurrentAt(t time.Time) bool {
	if t.Before(m.ValidFrom) {
		return false
	}
	return m.ValidTo == nil || t.Before(*m.ValidTo)
}

// IsCurrent reports whether the memory has not been superseded.
func (m *Memory) IsCurrent() bool { return m.ValidTo == nil }

// TopicKey is the supersession key: two memories collide only if they are the
// same tenant, the same kind, and the same normalized topic. Kind is part of the
// key on purpose — a decision about deploy cadence must not invalidate a fact
// about deploy cadence.
func (m *Memory) TopicKey() string {
	return m.TenantID + "\x00" + string(m.Kind) + "\x00" + m.Topic
}

// Expertise is a person-to-topic edge, accumulated from the memories a person
// authored or was a subject of. It powers "who knows about X".
type Expertise struct {
	TenantID    string    `json:"tenant_id"`
	PersonKey   string    `json:"person_key"`
	DisplayName string    `json:"display_name,omitempty"`
	Topic       string    `json:"topic"`
	Score       float64   `json:"score"`
	Signals     int       `json:"signals"`
	LastSeenAt  time.Time `json:"last_seen_at"`
}
