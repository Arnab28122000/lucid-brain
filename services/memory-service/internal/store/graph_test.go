package store_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/cortex-ai/cortex/services/memory-service/internal/memory"
	"github.com/cortex-ai/cortex/services/memory-service/internal/store"
)

// The graph is a projection of rows that already committed, so the failure mode
// worth testing is not "is the Cypher valid" but "did anything actually land".
// A silently no-op projection looks identical to a healthy one from the
// relational side.
func TestGraphProjectionIsPopulated(t *testing.T) {
	if os.Getenv("MEMORY_TEST_GRAPH") != "1" {
		t.Skip("MEMORY_TEST_GRAPH not set; skipping AGE projection test")
	}
	st, tenant := testStore(t)
	ctx := context.Background()
	acl := []string{"g-eng"}

	jan := time.Date(2026, 1, 10, 9, 0, 0, 0, time.UTC)
	mar := time.Date(2026, 3, 12, 9, 0, 0, 0, time.UTC)

	if _, err := st.Commit(ctx, episode(tenant, "g-1", "confluence", jan), []store.Proposal{
		proposal(tenant, memory.KindDecision, "graph test topic", "The team decided to use option A.", jan, acl),
	}, nil); err != nil {
		t.Fatal(err)
	}
	second := proposal(tenant, memory.KindDecision, "graph test topic", "The team decided to switch to option B.", mar, acl)
	second.Memory.Subjects = []string{"U-marco"}
	if _, err := st.Commit(ctx, episode(tenant, "g-2", "slack", mar), []store.Proposal{second}, nil); err != nil {
		t.Fatal(err)
	}

	topicKey := tenant + ":" + memory.NormalizeTopic("graph test topic")

	counts := map[string]string{
		"memories about the topic": `MATCH (m:Memory)-[:ABOUT]->(t:Topic {key: "` + topicKey + `"}) RETURN m`,
		"supersession edges":       `MATCH (n:Memory)-[:SUPERSEDES]->(o:Memory)-[:ABOUT]->(t:Topic {key: "` + topicKey + `"}) RETURN n`,
		"provenance edges":         `MATCH (m:Memory)-[:ABOUT]->(t:Topic {key: "` + topicKey + `"}) MATCH (m)-[:DERIVED_FROM]->(e:Episode) RETURN e`,
		"expertise edges":          `MATCH (p:Person)-[:EXPERT_IN]->(t:Topic {key: "` + topicKey + `"}) RETURN p`,
		"mentions edges":           `MATCH (m:Memory)-[:MENTIONS]->(p:Person) MATCH (m)-[:ABOUT]->(t:Topic {key: "` + topicKey + `"}) RETURN p`,
	}
	want := map[string]int{
		"memories about the topic": 2,
		"supersession edges":       1,
		"provenance edges":         2,
		"expertise edges":          2, // the author and the named subject
		"mentions edges":           1,
	}

	for label, query := range counts {
		var n int
		err := st.Pool().QueryRow(ctx,
			`SELECT count(*) FROM cypher('cortex_memory', $cypher$ `+query+` $cypher$) AS (v agtype)`).Scan(&n)
		if err != nil {
			t.Fatalf("%s: query failed: %v", label, err)
		}
		if n != want[label] {
			t.Errorf("%s: got %d, want %d", label, n, want[label])
		}
	}
}
