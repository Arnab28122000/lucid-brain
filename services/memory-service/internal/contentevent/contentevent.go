// Package contentevent defines the wire contract the ingestion plane publishes
// on NATS JetStream and its mapping into the memory domain.
//
// Memory extraction is a *subscriber* of this stream, not a stage in the
// ingestion pipeline: it runs asynchronously off the same events that drive
// parse → chunk → embed, so a slow LLM extraction can never stall indexing.
package contentevent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cortex-ai/cortex/services/memory-service/internal/memory"
)

// Subject is the JetStream subject pattern the ingestion plane publishes on.
// Tokens are tenant and source type, so a deployment can shard consumers per
// tenant without republishing.
const (
	SubjectPrefix  = "cortex.content"
	SubjectPattern = SubjectPrefix + ".>" // cortex.content.<tenant>.<source>.<op>
	StreamName     = "CORTEX_CONTENT"
)

// Op distinguishes content that appeared or changed from content that was
// removed at the source. Deletes drive memory invalidation and the
// right-to-be-forgotten path; they are never silent no-ops.
type Op string

const (
	OpUpsert Op = "upsert"
	OpDelete Op = "delete"
)

// Person mirrors the ingestion plane's actor representation.
type Person struct {
	ExternalID  string `json:"external_id"`
	DisplayName string `json:"display_name,omitempty"`
	Email       string `json:"email,omitempty"`
}

func (p Person) toDomain() memory.Person {
	return memory.Person{ExternalID: p.ExternalID, DisplayName: p.DisplayName, Email: p.Email}
}

// ContentEvent announces one normalized Document from a source system. It is
// keyed by ContentHash (SHA-256 of the normalized body) so re-ingestion is
// idempotent — replaying the whole stream must not double-extract memories.
type ContentEvent struct {
	EventID     string `json:"event_id"`
	TenantID    string `json:"tenant_id"`
	DocumentID  string `json:"document_id"`
	ContentHash string `json:"content_hash"`

	Op         Op     `json:"op"`
	SourceType string `json:"source_type"`
	SourceID   string `json:"source_id"`
	Permalink  string `json:"permalink"`
	Title      string `json:"title"`
	Body       string `json:"body"`

	Author       Person   `json:"author"`
	Participants []Person `json:"participants,omitempty"`
	ACLGroupIDs  []string `json:"acl_group_ids,omitempty"`

	// OccurredAt is world time — when the message was sent or the page edited.
	// It anchors ValidFrom, so a backfilled 2023 thread does not present itself
	// as today's truth.
	OccurredAt time.Time `json:"occurred_at"`
	// IngestedAt is system time — when Cortex saw it.
	IngestedAt time.Time `json:"ingested_at"`
}

// ErrSkip signals a well-formed event that carries nothing to extract. It is a
// successful outcome, not a failure: the message is acked and dropped.
var ErrSkip = errors.New("contentevent: nothing to extract")

// minBodyRunes filters out reactions, joins, and one-word replies, which cost a
// full LLM round trip and yield nothing.
const minBodyRunes = 80

// Validate checks the invariants the pipeline depends on before any LLM spend.
func (e *ContentEvent) Validate() error {
	switch {
	case e.EventID == "":
		return errors.New("contentevent: event_id required")
	case e.TenantID == "":
		return errors.New("contentevent: tenant_id required")
	case e.DocumentID == "":
		return errors.New("contentevent: document_id required")
	case e.ContentHash == "":
		return errors.New("contentevent: content_hash required")
	case e.Op != OpUpsert && e.Op != OpDelete:
		return fmt.Errorf("contentevent: unknown op %q", e.Op)
	case e.OccurredAt.IsZero():
		return errors.New("contentevent: occurred_at required")
	}
	return nil
}

// Episode converts the event into the provenance anchor memories are derived
// from. It returns ErrSkip for deletes and for bodies too thin to be worth an
// extraction pass.
func (e *ContentEvent) Episode() (memory.Episode, error) {
	if err := e.Validate(); err != nil {
		return memory.Episode{}, err
	}
	if e.Op == OpDelete {
		return memory.Episode{}, ErrSkip
	}
	body := strings.TrimSpace(e.Body)
	if len([]rune(body)) < minBodyRunes {
		return memory.Episode{}, ErrSkip
	}

	ingested := e.IngestedAt
	if ingested.IsZero() {
		ingested = time.Now().UTC()
	}
	participants := make([]memory.Person, 0, len(e.Participants))
	for _, p := range e.Participants {
		participants = append(participants, p.toDomain())
	}

	return memory.Episode{
		// The episode ID is the content hash, not the event ID: two events
		// carrying identical content are the same episode, which is what makes
		// stream replay idempotent all the way through provenance.
		ID:           e.TenantID + ":" + e.ContentHash,
		TenantID:     e.TenantID,
		DocumentID:   e.DocumentID,
		ContentHash:  e.ContentHash,
		SourceType:   e.SourceType,
		SourceID:     e.SourceID,
		Permalink:    e.Permalink,
		Title:        strings.TrimSpace(e.Title),
		Body:         body,
		Author:       e.Author.toDomain(),
		Participants: participants,
		ACLGroupIDs:  e.ACLGroupIDs,
		OccurredAt:   e.OccurredAt.UTC(),
		IngestedAt:   ingested.UTC(),
	}, nil
}

// Decode parses a JetStream message payload.
func Decode(data []byte) (*ContentEvent, error) {
	var e ContentEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("contentevent: decode: %w", err)
	}
	return &e, nil
}
