package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/cortex-ai/cortex/services/memory-service/internal/memory"
)

// Graph is the property-graph projection of the memory layer. It is written in
// the same transaction as the relational rows, so the two commit together or
// not at all — a projection that can drift is a projection nobody trusts.
type Graph interface {
	UpsertMemory(ctx context.Context, tx pgx.Tx, m memory.Memory) error
	UpsertEpisode(ctx context.Context, tx pgx.Tx, ep memory.Episode) error
	LinkDerivedFrom(ctx context.Context, tx pgx.Tx, memoryID, episodeID string) error
	LinkSupersedes(ctx context.Context, tx pgx.Tx, newerID, olderID string) error
	LinkExpertise(ctx context.Context, tx pgx.Tx, tenantID, personKey, displayName, topic string, weight float64) error
}

// NoopGraph satisfies Graph when AGE is not installed. Every v1 query is
// answerable from the relational tables alone, so running without the graph is
// a supported configuration rather than a degraded one.
type NoopGraph struct{}

func (NoopGraph) UpsertMemory(context.Context, pgx.Tx, memory.Memory) error   { return nil }
func (NoopGraph) UpsertEpisode(context.Context, pgx.Tx, memory.Episode) error { return nil }
func (NoopGraph) LinkDerivedFrom(context.Context, pgx.Tx, string, string) error {
	return nil
}
func (NoopGraph) LinkSupersedes(context.Context, pgx.Tx, string, string) error { return nil }
func (NoopGraph) LinkExpertise(context.Context, pgx.Tx, string, string, string, string, float64) error {
	return nil
}

// AGEGraph writes Cypher through Apache AGE.
type AGEGraph struct {
	GraphName string
}

// cypher runs a parameterless Cypher statement. AGE's parameter binding is
// awkward from a general SQL driver — it wants an agtype map, not positional
// args — so values are rendered into the query with a strict JSON-based
// quoter below. Every value that reaches it is either a server-generated UUID
// or a normalized key, and quoteAGE rejects anything it cannot escape.
func (g AGEGraph) cypher(ctx context.Context, tx pgx.Tx, query string) error {
	sql := fmt.Sprintf(`SELECT * FROM cypher('%s', $cypher$ %s $cypher$) AS (result agtype)`, g.GraphName, query)
	if _, err := tx.Exec(ctx, sql); err != nil {
		return fmt.Errorf("store: age cypher: %w", err)
	}
	return nil
}

// quoteAGE renders a Go string as a Cypher string literal. It goes through
// encoding/json so quotes, backslashes and control characters are escaped by
// code that is already correct, then rejects the one sequence that could break
// out of the dollar-quoted SQL wrapper above.
func quoteAGE(s string) (string, error) {
	if strings.Contains(s, "$cypher$") {
		return "", fmt.Errorf("store: refusing to render value containing the cypher delimiter")
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("store: quote value: %w", err)
	}
	return string(b), nil
}

func (g AGEGraph) UpsertMemory(ctx context.Context, tx pgx.Tx, m memory.Memory) error {
	id, err := quoteAGE(m.ID)
	if err != nil {
		return err
	}
	tenant, err := quoteAGE(m.TenantID)
	if err != nil {
		return err
	}
	kind, err := quoteAGE(string(m.Kind))
	if err != nil {
		return err
	}
	topicKey, err := quoteAGE(m.TenantID + ":" + m.Topic)
	if err != nil {
		return err
	}
	topic, err := quoteAGE(m.Topic)
	if err != nil {
		return err
	}

	// AGE has no ON CREATE SET / ON MATCH SET (through 1.7), so properties are
	// assigned with a plain SET after the MERGE. That is idempotent here
	// because every property written is derived from the row being projected —
	// re-running it rewrites the same values.
	q := fmt.Sprintf(`
		MERGE (t:Topic {key: %s})
		SET t.tenant_id = %s, t.topic = %s
		MERGE (m:Memory {key: %s})
		SET m.tenant_id = %s, m.kind = %s, m.topic = %s
		MERGE (m)-[:ABOUT]->(t)`,
		topicKey, tenant, topic, id, tenant, kind, topic)
	if err := g.cypher(ctx, tx, q); err != nil {
		return err
	}

	// MENTIONS edges are what make "which decisions is this person entangled
	// with" a one-hop query instead of an array scan.
	for _, subj := range m.Subjects {
		personKey, err := quoteAGE(m.TenantID + ":" + subj)
		if err != nil {
			return err
		}
		person, err := quoteAGE(subj)
		if err != nil {
			return err
		}
		q := fmt.Sprintf(`
			MATCH (m:Memory {key: %s})
			MERGE (p:Person {key: %s})
			SET p.tenant_id = %s, p.person_key = %s
			MERGE (m)-[:MENTIONS]->(p)`, id, personKey, tenant, person)
		if err := g.cypher(ctx, tx, q); err != nil {
			return err
		}
	}
	return nil
}

func (g AGEGraph) UpsertEpisode(ctx context.Context, tx pgx.Tx, ep memory.Episode) error {
	key, err := quoteAGE(ep.ID)
	if err != nil {
		return err
	}
	tenant, err := quoteAGE(ep.TenantID)
	if err != nil {
		return err
	}
	source, err := quoteAGE(ep.SourceType)
	if err != nil {
		return err
	}
	permalink, err := quoteAGE(ep.Permalink)
	if err != nil {
		return err
	}
	return g.cypher(ctx, tx, fmt.Sprintf(`
		MERGE (e:Episode {key: %s})
		SET e.tenant_id = %s, e.source_type = %s, e.permalink = %s`,
		key, tenant, source, permalink))
}

func (g AGEGraph) LinkDerivedFrom(ctx context.Context, tx pgx.Tx, memoryID, episodeID string) error {
	m, err := quoteAGE(memoryID)
	if err != nil {
		return err
	}
	e, err := quoteAGE(episodeID)
	if err != nil {
		return err
	}
	return g.cypher(ctx, tx, fmt.Sprintf(`
		MATCH (m:Memory {key: %s}), (e:Episode {key: %s})
		MERGE (m)-[:DERIVED_FROM]->(e)`, m, e))
}

func (g AGEGraph) LinkSupersedes(ctx context.Context, tx pgx.Tx, newerID, olderID string) error {
	newer, err := quoteAGE(newerID)
	if err != nil {
		return err
	}
	older, err := quoteAGE(olderID)
	if err != nil {
		return err
	}
	// Direction is newer -> older, so walking out from the current memory
	// replays history in reverse, which is the order a timeline renders in.
	return g.cypher(ctx, tx, fmt.Sprintf(`
		MATCH (n:Memory {key: %s}), (o:Memory {key: %s})
		MERGE (n)-[:SUPERSEDES]->(o)`, newer, older))
}

func (g AGEGraph) LinkExpertise(ctx context.Context, tx pgx.Tx, tenantID, personKey, displayName, topic string, weight float64) error {
	pKey, err := quoteAGE(tenantID + ":" + personKey)
	if err != nil {
		return err
	}
	tKey, err := quoteAGE(tenantID + ":" + topic)
	if err != nil {
		return err
	}
	tenant, err := quoteAGE(tenantID)
	if err != nil {
		return err
	}
	person, err := quoteAGE(personKey)
	if err != nil {
		return err
	}
	name, err := quoteAGE(displayName)
	if err != nil {
		return err
	}
	topicVal, err := quoteAGE(topic)
	if err != nil {
		return err
	}
	// The edge score accumulates across calls, so unlike the vertex properties
	// above this SET is *not* idempotent — coalesce handles the first write,
	// and re-running the same expertise signal would double-count it. That is
	// acceptable because it is only ever called from inside the commit
	// transaction, which is itself guarded by the ingest log.
	return g.cypher(ctx, tx, fmt.Sprintf(`
		MERGE (p:Person {key: %s})
		SET p.tenant_id = %s, p.person_key = %s, p.display_name = %s
		MERGE (t:Topic {key: %s})
		SET t.tenant_id = %s, t.topic = %s
		MERGE (p)-[r:EXPERT_IN]->(t)
		SET r.score = coalesce(r.score, 0) + %f`,
		pKey, tenant, person, name, tKey, tenant, topicVal, weight))
}
