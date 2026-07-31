package contentevent

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func validEvent() *ContentEvent {
	return &ContentEvent{
		EventID:     "evt-1",
		TenantID:    "acme",
		DocumentID:  "doc-1",
		ContentHash: "abc123",
		Op:          OpUpsert,
		SourceType:  "slack",
		Permalink:   "https://slack.example/archives/C1/p1",
		Body:        strings.Repeat("a real thread with enough content to be worth extracting. ", 3),
		OccurredAt:  time.Date(2026, 3, 12, 10, 0, 0, 0, time.UTC),
		IngestedAt:  time.Date(2026, 3, 12, 10, 1, 0, 0, time.UTC),
		ACLGroupIDs: []string{"g-eng"},
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ContentEvent)
	}{
		{"missing event_id", func(e *ContentEvent) { e.EventID = "" }},
		{"missing tenant_id", func(e *ContentEvent) { e.TenantID = "" }},
		{"missing document_id", func(e *ContentEvent) { e.DocumentID = "" }},
		{"missing content_hash", func(e *ContentEvent) { e.ContentHash = "" }},
		{"unknown op", func(e *ContentEvent) { e.Op = "mutate" }},
		{"missing occurred_at", func(e *ContentEvent) { e.OccurredAt = time.Time{} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := validEvent()
			tc.mutate(e)
			if err := e.Validate(); err == nil {
				t.Error("Validate() = nil, want an error before any LLM spend")
			}
		})
	}
	if err := validEvent().Validate(); err != nil {
		t.Errorf("valid event rejected: %v", err)
	}
}

func TestEpisodeIsKeyedByContentHash(t *testing.T) {
	// Two events carrying identical content are the same episode. This is what
	// makes stream replay idempotent all the way through provenance.
	a, err := validEvent().Episode()
	if err != nil {
		t.Fatal(err)
	}
	second := validEvent()
	second.EventID = "evt-2"
	b, err := second.Episode()
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Errorf("same content produced different episode IDs: %q vs %q", a.ID, b.ID)
	}
	if !strings.Contains(a.ID, "abc123") {
		t.Errorf("episode ID %q does not incorporate the content hash", a.ID)
	}
}

func TestEpisodeSkipsDeletesAndThinBodies(t *testing.T) {
	del := validEvent()
	del.Op = OpDelete
	if _, err := del.Episode(); !errors.Is(err, ErrSkip) {
		t.Errorf("delete: got %v, want ErrSkip — deletes take the invalidation path, not extraction", err)
	}

	thin := validEvent()
	thin.Body = "👍"
	if _, err := thin.Episode(); !errors.Is(err, ErrSkip) {
		t.Errorf("reaction-only body: got %v, want ErrSkip", err)
	}
}

func TestEpisodeCarriesACLsAndTiming(t *testing.T) {
	ep, err := validEvent().Episode()
	if err != nil {
		t.Fatal(err)
	}
	if len(ep.ACLGroupIDs) != 1 || ep.ACLGroupIDs[0] != "g-eng" {
		t.Errorf("ACLs lost in conversion: %v — memories would be invisible or, worse, unfiltered", ep.ACLGroupIDs)
	}
	if !ep.OccurredAt.Equal(time.Date(2026, 3, 12, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("occurred_at = %v", ep.OccurredAt)
	}
	if ep.IngestedAt.Before(ep.OccurredAt) {
		t.Error("ingested_at must not precede occurred_at for a normally-delivered event")
	}
}

func TestEpisodeDefaultsIngestedAt(t *testing.T) {
	e := validEvent()
	e.IngestedAt = time.Time{}
	ep, err := e.Episode()
	if err != nil {
		t.Fatal(err)
	}
	if ep.IngestedAt.IsZero() {
		t.Error("ingested_at must be defaulted; system time is what audit queries ask about")
	}
}

func TestDecode(t *testing.T) {
	payload := []byte(`{"event_id":"e1","tenant_id":"acme","document_id":"d1","content_hash":"h1","op":"upsert","occurred_at":"2026-03-12T10:00:00Z"}`)
	ev, err := Decode(payload)
	if err != nil {
		t.Fatal(err)
	}
	if ev.EventID != "e1" || ev.Op != OpUpsert {
		t.Errorf("decoded event = %+v", ev)
	}
	if _, err := Decode([]byte("not json")); err == nil {
		t.Error("Decode of garbage = nil error, want an error so the message is terminated rather than retried")
	}
}
