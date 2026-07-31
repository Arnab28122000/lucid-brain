package pipeline_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cortex-ai/cortex/services/memory-service/internal/extract"
	"github.com/cortex-ai/cortex/services/memory-service/internal/llm"
	"github.com/cortex-ai/cortex/services/memory-service/internal/memory"
	"github.com/cortex-ai/cortex/services/memory-service/internal/pipeline"
	"github.com/cortex-ai/cortex/services/memory-service/internal/store"
	"github.com/cortex-ai/cortex/services/memory-service/migrations"
)

// End-to-end over the real store with a scripted model: extraction and
// verification are already unit-tested in isolation, so what this covers is the
// wiring between them and the bi-temporal writes underneath.

const threadBody = `Priya: We went back and forth on this, but we have decided to move the Enterprise plan to $40 per seat per month starting 2026-04-01.
Marco: Agreed. That replaces the $32 we published on the pricing page in January.
Priya: I will update the pricing page by Friday.`

const broadJSON = `{"memories":[
 {"kind":"decision","topic":"Enterprise plan pricing","statement":"Priya and Marco decided the Enterprise plan moves to $40 per seat per month from 2026-04-01.","subjects":["Priya","Marco"],"confidence":0.92,"quote":"we have decided to move the Enterprise plan to $40 per seat per month starting 2026-04-01"},
 {"kind":"fact","topic":"pricing page state","statement":"The pricing page published a $32 Enterprise price in January 2026.","confidence":0.85,"quote":"That replaces the $32 we published on the pricing page in January"},
 {"kind":"decision","topic":"vague","statement":"They agreed on it.","confidence":0.9,"quote":"Agreed."}
]}`

const detailJSON = `{"details":[
 {"kind":"decision","topic":"Enterprise plan pricing","statement":"The Enterprise plan price becomes $40 per seat per month on 2026-04-01.","attributes":{"price":"$40 per seat per month","effective":"2026-04-01"},"confidence":0.95,"quote":"$40 per seat per month starting 2026-04-01"},
 {"kind":"fact","topic":"invented discount","statement":"The Enterprise plan includes a 25% volume discount.","attributes":{"discount":"25%"},"confidence":0.9,"quote":"a 25% volume discount applies"}
]}`

func testPipeline(t *testing.T, responses []string) (*pipeline.Pipeline, *store.Store, string) {
	t.Helper()
	dsn := os.Getenv("MEMORY_TEST_DSN")
	if dsn == "" {
		t.Skip("MEMORY_TEST_DSN not set; skipping pipeline integration tests")
	}

	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	all, err := migrations.All()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range all {
		if strings.Contains(m.Name, "age") {
			continue
		}
		if _, err := st.Pool().Exec(ctx, m.SQL); err != nil {
			t.Fatalf("apply migration %s: %v", m.Name, err)
		}
	}

	p := pipeline.New(st, extract.New(&llm.Fake{Responses: responses}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	tenant := fmt.Sprintf("p-%d", time.Now().UnixNano())
	return p, st, tenant
}

func testEpisode(tenant, hash string) memory.Episode {
	at := time.Date(2026, 3, 12, 14, 30, 0, 0, time.UTC)
	return memory.Episode{
		ID:           tenant + ":" + hash,
		TenantID:     tenant,
		DocumentID:   "doc-" + hash,
		ContentHash:  hash,
		SourceType:   "slack",
		Permalink:    "https://slack.example/archives/C1/p1",
		Title:        "#pricing",
		Body:         threadBody,
		Author:       memory.Person{ExternalID: "U-priya", DisplayName: "Priya"},
		Participants: []memory.Person{{ExternalID: "U-marco", DisplayName: "Marco"}},
		ACLGroupIDs:  []string{"g-pricing"},
		OccurredAt:   at,
		IngestedAt:   at.Add(time.Minute),
	}
}

func TestProcessExtractsVerifiesAndCommits(t *testing.T) {
	p, st, tenant := testPipeline(t, []string{broadJSON, detailJSON})
	ctx := context.Background()

	rep, err := p.Process(ctx, testEpisode(tenant, "h1"))
	if err != nil {
		t.Fatalf("process: %v", err)
	}

	if rep.Candidates != 4 {
		t.Errorf("candidates = %d, want 4 (3 broad minus 1 unmergeable, plus detail)", rep.Candidates)
	}
	// The vague decision fails self-containment; the invented 25% discount
	// fails attribute fidelity. Both must be rejected before they reach a row.
	if rep.Rejected < 2 {
		t.Errorf("rejected = %d, want at least 2 (the vague decision and the hallucinated discount)", rep.Rejected)
	}
	if rep.Accepted < 2 {
		t.Fatalf("accepted = %d, want at least 2 real memories", rep.Accepted)
	}

	mems, err := st.Search(ctx, store.SearchFilter{TenantID: tenant, ACLGroupIDs: []string{"g-pricing"}, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range mems {
		if strings.Contains(m.Statement, "25%") {
			t.Error("a hallucinated attribute reached the store")
		}
		if strings.Contains(m.Statement, "They agreed") {
			t.Error("an unresolvable statement reached the store")
		}
		if len(m.Sources) == 0 {
			t.Errorf("memory %q has no provenance and cannot be cited", m.Statement)
		}
		if len(m.ACLGroupIDs) == 0 {
			t.Errorf("memory %q carries no ACLs", m.Statement)
		}
	}

	decisions, err := st.Search(ctx, store.SearchFilter{
		TenantID: tenant, ACLGroupIDs: []string{"g-pricing"},
		Kinds: []memory.Kind{memory.KindDecision}, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 {
		t.Fatalf("got %d decisions, want 1", len(decisions))
	}
	if got := decisions[0].Attributes["price"]; got != "$40 per seat per month" {
		t.Errorf("detail attributes did not survive to the store: %v", decisions[0].Attributes)
	}
}

func TestProcessIsIdempotentOnRedelivery(t *testing.T) {
	p, st, tenant := testPipeline(t, []string{broadJSON, detailJSON})
	ctx := context.Background()
	ep := testEpisode(tenant, "h2")

	if _, err := p.Process(ctx, ep); err != nil {
		t.Fatal(err)
	}
	before, err := st.Stats(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}

	rep, err := p.Process(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Skipped {
		t.Error("a redelivered episode was reprocessed; JetStream is at-least-once and extraction costs an LLM call")
	}

	after, err := st.Stats(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if after.Total != before.Total {
		t.Errorf("memory count changed on redelivery: %d -> %d", before.Total, after.Total)
	}
}

func TestProcessFailsRatherThanMarkingAnEpisodeDoneWhenTheModelIsDown(t *testing.T) {
	dsn := os.Getenv("MEMORY_TEST_DSN")
	if dsn == "" {
		t.Skip("MEMORY_TEST_DSN not set")
	}
	p, st, tenant := testPipeline(t, nil)
	ctx := context.Background()

	// Swap in a client that always fails, as an unreachable llm-gateway would.
	p.Extractor = extract.New(&llm.Fake{Err: llm.ErrUnavailable})
	ep := testEpisode(tenant, "h3")

	if _, err := p.Process(ctx, ep); err == nil {
		t.Fatal("process succeeded with an unreachable model; the episode would be acked and lost")
	}

	done, err := st.AlreadyProcessed(ctx, tenant, ep.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Error("the episode was marked processed despite failing; redelivery would skip it forever")
	}
}
