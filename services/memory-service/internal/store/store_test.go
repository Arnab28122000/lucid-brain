package store_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cortex-ai/cortex/services/memory-service/internal/memory"
	"github.com/cortex-ai/cortex/services/memory-service/internal/store"
	"github.com/cortex-ai/cortex/services/memory-service/migrations"
)

// These are integration tests: the bi-temporal invariants they cover live half
// in Go and half in SQL (the partial unique index, the interval constraint, the
// FOR UPDATE lock), so testing them against a fake store would test nothing that
// matters. Set MEMORY_TEST_DSN to run them.
//
//	docker run -d -p 5433:5432 -e POSTGRES_PASSWORD=postgres apache/age:latest
//	MEMORY_TEST_DSN='postgres://postgres:postgres@localhost:5433/postgres' go test ./...

func testStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dsn := os.Getenv("MEMORY_TEST_DSN")
	if dsn == "" {
		t.Skip("MEMORY_TEST_DSN not set; skipping store integration tests")
	}

	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{DSN: dsn, GraphEnabled: os.Getenv("MEMORY_TEST_GRAPH") == "1"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	all, err := migrations.All()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	for _, m := range all {
		if strings.Contains(m.Name, "age") && os.Getenv("MEMORY_TEST_GRAPH") != "1" {
			continue
		}
		if _, err := st.Pool().Exec(ctx, m.SQL); err != nil {
			t.Fatalf("apply migration %s: %v", m.Name, err)
		}
	}

	// Each test gets its own tenant, so they can run against a shared database
	// without a truncate step racing between them.
	tenant := fmt.Sprintf("t-%s-%d", strings.ToLower(strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())), time.Now().UnixNano())
	return st, tenant
}

func episode(tenant, hash, source string, at time.Time, acl ...string) memory.Episode {
	if len(acl) == 0 {
		acl = []string{"g-eng"}
	}
	return memory.Episode{
		ID:          tenant + ":" + hash,
		TenantID:    tenant,
		DocumentID:  "doc-" + hash,
		ContentHash: hash,
		SourceType:  source,
		Permalink:   "https://" + source + ".example/" + hash,
		Title:       source + " " + hash,
		Body:        "source body for " + hash,
		Author:      memory.Person{ExternalID: "U-priya", DisplayName: "Priya"},
		ACLGroupIDs: acl,
		OccurredAt:  at,
		IngestedAt:  at.Add(time.Minute),
	}
}

func proposal(tenant string, kind memory.Kind, topicRaw, statement string, validFrom time.Time, acl []string) store.Proposal {
	return store.Proposal{
		Memory: memory.Memory{
			TenantID:    tenant,
			Kind:        kind,
			Topic:       memory.NormalizeTopic(topicRaw),
			TopicRaw:    topicRaw,
			Statement:   statement,
			Confidence:  0.9,
			Salience:    1,
			ValidFrom:   validFrom,
			IngestedAt:  time.Now().UTC(),
			ACLGroupIDs: acl,
			LastSeenAt:  validFrom,
		},
		Quote: statement,
		Pass:  "broad",
	}
}

// The worked example from the design: a Confluence page states the deploy
// policy, a later Slack thread reverses it. The older memory must be closed off
// at the decision's timestamp and linked forward — not deleted, because the
// answer has to be able to show what it replaced.
func TestSlackDecisionSupersedesStaleConfluencePage(t *testing.T) {
	st, tenant := testStore(t)
	ctx := context.Background()

	jan := time.Date(2026, 1, 10, 9, 0, 0, 0, time.UTC)
	mar := time.Date(2026, 3, 12, 14, 30, 0, 0, time.UTC)
	acl := []string{"g-eng"}

	confluence := episode(tenant, "conf-1", "confluence", jan)
	if _, err := st.Commit(ctx, confluence, []store.Proposal{
		proposal(tenant, memory.KindDecision, "deploy freeze policy",
			"The team decided to freeze deploys every Friday.", jan, acl),
	}, nil); err != nil {
		t.Fatalf("commit confluence: %v", err)
	}

	slack := episode(tenant, "slack-1", "slack", mar)
	res, err := st.Commit(ctx, slack, []store.Proposal{
		proposal(tenant, memory.KindDecision, "Deploy freeze policy",
			"The team decided to lift the Friday deploy freeze and allow deploys any weekday.", mar, acl),
	}, nil)
	if err != nil {
		t.Fatalf("commit slack: %v", err)
	}

	if got := res.Outcomes[store.OutcomeSuperseded]; got != 1 {
		t.Fatalf("outcomes = %v, want one supersession", res.Outcomes)
	}
	if len(res.Superseded) != 1 || !res.Superseded[0].ValidTo.Equal(mar) {
		t.Fatalf("supersession = %+v, want valid_to at the decision timestamp %s", res.Superseded, mar)
	}

	// What is true now is the Slack decision.
	current, err := st.Search(ctx, store.SearchFilter{TenantID: tenant, ACLGroupIDs: acl})
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 {
		t.Fatalf("got %d current memories, want 1", len(current))
	}
	if !strings.Contains(current[0].Statement, "lift the Friday deploy freeze") {
		t.Errorf("current memory is the stale one: %q", current[0].Statement)
	}
	if len(current[0].Sources) == 0 || current[0].Sources[0].SourceType != "slack" {
		t.Errorf("current memory is not citable back to Slack: %+v", current[0].Sources)
	}

	// What was true in February is still the Confluence page.
	feb := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	asOf, err := st.Search(ctx, store.SearchFilter{TenantID: tenant, ACLGroupIDs: acl, AsOf: &feb})
	if err != nil {
		t.Fatal(err)
	}
	if len(asOf) != 1 || !strings.Contains(asOf[0].Statement, "freeze deploys every Friday") {
		t.Fatalf("as-of February returned %+v, want the Confluence-derived memory", asOf)
	}

	// And the history is intact and linked, not deleted.
	history, err := st.History(ctx, tenant, memory.KindDecision, "deploy freeze policy", acl, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("history has %d entries, want 2 — supersession must never delete", len(history))
	}
	if history[0].SupersededBy == nil || *history[0].SupersededBy != history[1].ID {
		t.Error("the old memory is not linked forward to its replacement")
	}
	if history[0].ValidTo == nil || !history[0].ValidTo.Equal(mar) {
		t.Errorf("old memory valid_to = %v, want %v", history[0].ValidTo, mar)
	}
}

func TestRestatementReinforcesRatherThanSuperseding(t *testing.T) {
	st, tenant := testStore(t)
	ctx := context.Background()
	acl := []string{"g-eng"}

	jan := time.Date(2026, 1, 10, 9, 0, 0, 0, time.UTC)
	feb := time.Date(2026, 2, 10, 9, 0, 0, 0, time.UTC)
	stmt := "The Enterprise plan costs $40 per seat per month."

	if _, err := st.Commit(ctx, episode(tenant, "e1", "slack", jan),
		[]store.Proposal{proposal(tenant, memory.KindFact, "enterprise plan pricing", stmt, jan, acl)}, nil); err != nil {
		t.Fatal(err)
	}
	res, err := st.Commit(ctx, episode(tenant, "e2", "confluence", feb),
		[]store.Proposal{proposal(tenant, memory.KindFact, "pricing of the enterprise plan", stmt, feb, acl)}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if got := res.Outcomes[store.OutcomeReinforced]; got != 1 {
		t.Fatalf("outcomes = %v, want a reinforcement — the same claim from a second source is corroboration", res.Outcomes)
	}

	history, err := st.History(ctx, tenant, memory.KindFact, "enterprise plan pricing", acl, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Fatalf("history has %d entries, want 1 — a restatement must not write a new row", len(history))
	}
	if len(history[0].Sources) != 2 {
		t.Errorf("reinforced memory cites %d sources, want 2", len(history[0].Sources))
	}
	if history[0].Salience <= 1 {
		t.Errorf("salience = %v, want a bump from reinforcement", history[0].Salience)
	}
}

// Two near-identical sentences that disagree on a number are a reversal, not a
// restatement. This is the case a naive text-similarity check gets wrong.
func TestContradictoryAttributesForceSupersession(t *testing.T) {
	st, tenant := testStore(t)
	ctx := context.Background()
	acl := []string{"g-eng"}

	jan := time.Date(2026, 1, 10, 9, 0, 0, 0, time.UTC)
	feb := time.Date(2026, 2, 10, 9, 0, 0, 0, time.UTC)

	first := proposal(tenant, memory.KindFact, "enterprise plan pricing",
		"The Enterprise plan costs $40 per seat per month.", jan, acl)
	first.Memory.Attributes = map[string]string{"price": "$40"}

	second := proposal(tenant, memory.KindFact, "enterprise plan pricing",
		"The Enterprise plan costs $50 per seat per month.", feb, acl)
	second.Memory.Attributes = map[string]string{"price": "$50"}

	if _, err := st.Commit(ctx, episode(tenant, "e1", "slack", jan), []store.Proposal{first}, nil); err != nil {
		t.Fatal(err)
	}
	res, err := st.Commit(ctx, episode(tenant, "e2", "slack", feb), []store.Proposal{second}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if got := res.Outcomes[store.OutcomeSuperseded]; got != 1 {
		t.Fatalf("outcomes = %v, want a supersession: $40 and $50 cannot both be current", res.Outcomes)
	}
}

func TestBackfilledMemoryDoesNotDisturbTheCurrentOne(t *testing.T) {
	st, tenant := testStore(t)
	ctx := context.Background()
	acl := []string{"g-eng"}

	jan := time.Date(2026, 1, 10, 9, 0, 0, 0, time.UTC)
	mar := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)

	// The March memory lands first (a live channel), then a January thread is
	// backfilled behind it.
	if _, err := st.Commit(ctx, episode(tenant, "e-new", "slack", mar), []store.Proposal{
		proposal(tenant, memory.KindFact, "on-call rotation", "Marco owns the on-call rotation as of March 2026.", mar, acl),
	}, nil); err != nil {
		t.Fatal(err)
	}
	res, err := st.Commit(ctx, episode(tenant, "e-old", "slack", jan), []store.Proposal{
		proposal(tenant, memory.KindFact, "on-call rotation", "Priya owned the on-call rotation in January 2026.", jan, acl),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if got := res.Outcomes[store.OutcomeBackfilled]; got != 1 {
		t.Fatalf("outcomes = %v, want a backfill", res.Outcomes)
	}

	current, err := st.Search(ctx, store.SearchFilter{TenantID: tenant, ACLGroupIDs: acl})
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 || !strings.Contains(current[0].Statement, "Marco") {
		t.Fatalf("current = %+v, want the March memory to remain current", current)
	}

	janQuery := jan.Add(time.Hour)
	asOf, err := st.Search(ctx, store.SearchFilter{TenantID: tenant, ACLGroupIDs: acl, AsOf: &janQuery})
	if err != nil {
		t.Fatal(err)
	}
	if len(asOf) != 1 || !strings.Contains(asOf[0].Statement, "Priya") {
		t.Fatalf("as-of January = %+v, want the backfilled memory", asOf)
	}
}

func TestEventsNeverSupersede(t *testing.T) {
	st, tenant := testStore(t)
	ctx := context.Background()
	acl := []string{"g-eng"}

	base := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		at := base.Add(time.Duration(i) * time.Hour)
		if _, err := st.Commit(ctx, episode(tenant, fmt.Sprintf("ev-%d", i), "slack", at), []store.Proposal{
			proposal(tenant, memory.KindEvent, "checkout latency incident",
				fmt.Sprintf("Checkout latency alerted at %s.", at.Format(time.RFC3339)), at, acl),
		}, nil); err != nil {
			t.Fatal(err)
		}
	}

	mems, err := st.Search(ctx, store.SearchFilter{
		TenantID: tenant, ACLGroupIDs: acl, Kinds: []memory.Kind{memory.KindEvent}, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 3 {
		t.Fatalf("got %d events, want 3 — events are a stream and must all stay current", len(mems))
	}
}

func TestSearchEnforcesPermissionParity(t *testing.T) {
	st, tenant := testStore(t)
	ctx := context.Background()
	at := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)

	secret := []string{"g-finance"}
	if _, err := st.Commit(ctx, episode(tenant, "sec-1", "confluence", at, secret...), []store.Proposal{
		proposal(tenant, memory.KindFact, "acquisition target", "Acme is evaluating an acquisition of Globex.", at, secret),
	}, nil); err != nil {
		t.Fatal(err)
	}

	visible, err := st.Search(ctx, store.SearchFilter{TenantID: tenant, ACLGroupIDs: []string{"g-eng"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 0 {
		t.Fatalf("an engineer saw %d finance memories: %+v", len(visible), visible)
	}

	authorized, err := st.Search(ctx, store.SearchFilter{TenantID: tenant, ACLGroupIDs: secret})
	if err != nil {
		t.Fatal(err)
	}
	if len(authorized) != 1 {
		t.Fatalf("an authorized caller saw %d memories, want 1", len(authorized))
	}

	// An empty group set must return nothing rather than everything.
	none, err := st.Search(ctx, store.SearchFilter{TenantID: tenant})
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("a caller with no groups saw %d memories", len(none))
	}
}

func TestSearchIsTenantIsolated(t *testing.T) {
	st, tenant := testStore(t)
	ctx := context.Background()
	at := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	other := tenant + "-other"
	acl := []string{"g-eng"}

	if _, err := st.Commit(ctx, episode(other, "o-1", "slack", at), []store.Proposal{
		proposal(other, memory.KindFact, "deploy cadence", "The other tenant deploys weekly.", at, acl),
	}, nil); err != nil {
		t.Fatal(err)
	}

	mems, err := st.Search(ctx, store.SearchFilter{TenantID: tenant, ACLGroupIDs: acl})
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 0 {
		t.Fatalf("saw %d memories from another tenant sharing a group name", len(mems))
	}
}

func TestTimelineShowsWhatEachDecisionReplaced(t *testing.T) {
	st, tenant := testStore(t)
	ctx := context.Background()
	acl := []string{"g-eng"}

	jan := time.Date(2026, 1, 10, 9, 0, 0, 0, time.UTC)
	mar := time.Date(2026, 3, 12, 9, 0, 0, 0, time.UTC)

	if _, err := st.Commit(ctx, episode(tenant, "d1", "confluence", jan), []store.Proposal{
		proposal(tenant, memory.KindDecision, "vector database choice", "The team decided to use pgvector.", jan, acl),
	}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Commit(ctx, episode(tenant, "d2", "slack", mar), []store.Proposal{
		proposal(tenant, memory.KindDecision, "vector database choice", "The team decided to move to Qdrant.", mar, acl),
	}, nil); err != nil {
		t.Fatal(err)
	}

	entries, err := st.Timeline(ctx, store.TimelineFilter{TenantID: tenant, ACLGroupIDs: acl})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("timeline has %d entries, want 2 — a timeline that hides the reversal is just a list", len(entries))
	}
	newest := entries[0]
	if !newest.Current {
		t.Error("the newest decision should be marked current")
	}
	if newest.Replaced == nil {
		t.Fatal("the newest decision does not carry what it replaced")
	}
	if !strings.Contains(newest.Replaced.Statement, "pgvector") {
		t.Errorf("replaced = %q, want the pgvector decision", newest.Replaced.Statement)
	}
}

func TestExpertiseAccumulatesFromAuthorshipAndSubjects(t *testing.T) {
	st, tenant := testStore(t)
	ctx := context.Background()
	acl := []string{"g-eng"}
	at := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)

	ep := episode(tenant, "x1", "slack", at)
	ep.Participants = []memory.Person{{ExternalID: "U-marco", DisplayName: "Marco"}}

	p := proposal(tenant, memory.KindFact, "qdrant quantization",
		"Binary quantization is enabled on the primary Qdrant collection.", at, acl)
	p.Memory.Subjects = []string{"U-marco"}

	if _, err := st.Commit(ctx, ep, []store.Proposal{p}, nil); err != nil {
		t.Fatal(err)
	}

	experts, err := st.Experts(ctx, tenant, "qdrant quantization", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(experts) < 2 {
		t.Fatalf("got %d experts, want the author and the subject", len(experts))
	}
	if experts[0].PersonKey != "U-marco" {
		t.Errorf("top expert = %q, want the named subject to outrank the author", experts[0].PersonKey)
	}
}

func TestDecayArchivesColdFactsButNotDecisions(t *testing.T) {
	st, tenant := testStore(t)
	ctx := context.Background()
	acl := []string{"g-eng"}

	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := st.Commit(ctx, episode(tenant, "old-1", "slack", old), []store.Proposal{
		proposal(tenant, memory.KindFact, "old build tooling", "The build used Make in 2020.", old, acl),
		proposal(tenant, memory.KindDecision, "monorepo layout", "The team decided on a monorepo layout.", old, acl),
	}, nil); err != nil {
		t.Fatal(err)
	}

	res, err := st.ApplyDecay(ctx, tenant, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), 0.15)
	if err != nil {
		t.Fatal(err)
	}
	if res.Archived != 1 {
		t.Fatalf("archived %d memories, want exactly the cold fact (decisions are exempt)", res.Archived)
	}

	remaining, err := st.Search(ctx, store.SearchFilter{TenantID: tenant, ACLGroupIDs: acl})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].Kind != memory.KindDecision {
		t.Fatalf("remaining = %+v, want only the decision", remaining)
	}
}

func TestConsolidateRefusesToMergeAcrossPermissionBoundaries(t *testing.T) {
	st, tenant := testStore(t)
	ctx := context.Background()
	at := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)

	group := store.ConsolidationGroup{
		TenantID: tenant,
		Kind:     memory.KindEvent,
		Topic:    memory.NormalizeTopic("incident review"),
		Memories: []memory.Memory{
			{ID: "00000000-0000-0000-0000-000000000001", ACLGroupIDs: []string{"g-eng"}, ValidFrom: at},
			{ID: "00000000-0000-0000-0000-000000000002", ACLGroupIDs: []string{"g-finance"}, ValidFrom: at},
		},
	}
	if _, err := st.Consolidate(ctx, group, memory.Memory{Statement: "merged"}); err == nil {
		t.Fatal("consolidation across disjoint ACL groups succeeded; the summary would leak content across a permission boundary")
	}
}

func TestConsolidateMergesAndInheritsProvenance(t *testing.T) {
	st, tenant := testStore(t)
	ctx := context.Background()
	acl := []string{"g-eng"}
	base := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)

	for i := 0; i < 3; i++ {
		at := base.Add(time.Duration(i) * time.Hour)
		if _, err := st.Commit(ctx, episode(tenant, fmt.Sprintf("c-%d", i), "slack", at), []store.Proposal{
			proposal(tenant, memory.KindEvent, "checkout latency incident",
				fmt.Sprintf("Checkout latency exceeded the SLO at %s.", at.Format(time.RFC3339)), at, acl),
		}, nil); err != nil {
			t.Fatal(err)
		}
	}

	groups, err := st.FindConsolidationGroups(ctx, tenant, 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("found %d consolidation groups, want 1", len(groups))
	}

	id, err := st.Consolidate(ctx, groups[0], memory.Memory{
		Statement:  "Checkout latency exceeded the SLO three times on 2026-03-10.",
		TopicRaw:   "checkout latency incident",
		Confidence: 0.8,
	})
	if err != nil {
		t.Fatal(err)
	}

	merged, err := st.Get(ctx, tenant, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Sources) != 3 {
		t.Errorf("consolidated memory cites %d sources, want 3 — consolidation must not destroy citability", len(merged.Sources))
	}
	if len(merged.Supersedes) != 3 {
		t.Errorf("consolidated memory supersedes %d memories, want 3", len(merged.Supersedes))
	}

	current, err := st.Search(ctx, store.SearchFilter{TenantID: tenant, ACLGroupIDs: acl, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 {
		t.Fatalf("got %d current memories after consolidation, want 1", len(current))
	}
}

func TestInvalidateByDocumentKeepsCorroboratedMemories(t *testing.T) {
	st, tenant := testStore(t)
	ctx := context.Background()
	acl := []string{"g-eng"}

	jan := time.Date(2026, 1, 10, 9, 0, 0, 0, time.UTC)
	feb := time.Date(2026, 2, 10, 9, 0, 0, 0, time.UTC)
	stmt := "The Enterprise plan costs $40 per seat per month."

	// Two documents assert the same thing, so the memory is corroborated.
	epA := episode(tenant, "doc-a", "confluence", jan)
	if _, err := st.Commit(ctx, epA, []store.Proposal{
		proposal(tenant, memory.KindFact, "enterprise plan pricing", stmt, jan, acl),
	}, nil); err != nil {
		t.Fatal(err)
	}
	epB := episode(tenant, "doc-b", "slack", feb)
	if _, err := st.Commit(ctx, epB, []store.Proposal{
		proposal(tenant, memory.KindFact, "enterprise plan pricing", stmt, feb, acl),
	}, nil); err != nil {
		t.Fatal(err)
	}

	// A third memory from a second revision of doc-a, corroborated by nothing else.
	epA2 := episode(tenant, "doc-a2", "confluence", jan)
	epA2.DocumentID = epA.DocumentID
	if _, err := st.Commit(ctx, epA2, []store.Proposal{
		proposal(tenant, memory.KindFact, "legacy billing contact", "Billing questions go to the finance inbox.", jan, acl),
	}, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := st.InvalidateByDocument(ctx, tenant, epA.DocumentID, feb, false); err != nil {
		t.Fatal(err)
	}

	current, err := st.Search(ctx, store.SearchFilter{TenantID: tenant, ACLGroupIDs: acl, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var sawPricing bool
	for _, m := range current {
		if strings.Contains(m.Statement, "Enterprise plan") {
			sawPricing = true
		}
		if strings.Contains(m.Statement, "finance inbox") {
			t.Error("a memory whose only source was deleted is still current")
		}
	}
	if !sawPricing {
		t.Error("a memory corroborated by a surviving document was invalidated")
	}
}

func TestCommitIsIdempotentByContentHash(t *testing.T) {
	st, tenant := testStore(t)
	ctx := context.Background()
	acl := []string{"g-eng"}
	at := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)

	ep := episode(tenant, "dup-1", "slack", at)
	p := proposal(tenant, memory.KindFact, "deploy cadence", "The team deploys every Thursday.", at, acl)

	if _, err := st.Commit(ctx, ep, []store.Proposal{p}, nil); err != nil {
		t.Fatal(err)
	}

	done, err := st.AlreadyProcessed(ctx, tenant, ep.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("the ingest log did not record the episode; JetStream redelivery would re-extract it")
	}
}

func TestStatsReportsRejectionMix(t *testing.T) {
	st, tenant := testStore(t)
	ctx := context.Background()
	acl := []string{"g-eng"}
	at := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)

	if _, err := st.Commit(ctx, episode(tenant, "s-1", "slack", at),
		[]store.Proposal{proposal(tenant, memory.KindFact, "deploy cadence", "The team deploys every Thursday.", at, acl)},
		[]store.RejectionRecord{{
			Kind: "fact", Topic: "general", Statement: "Something vague.",
			FailedCheck: "topic", Reason: "topic: too generic",
		}}); err != nil {
		t.Fatal(err)
	}

	stats, err := st.Stats(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Current != 1 {
		t.Errorf("current = %d, want 1", stats.Current)
	}
	if stats.Rejections["topic"] != 1 {
		t.Errorf("rejections = %v, want one topic rejection", stats.Rejections)
	}
}
