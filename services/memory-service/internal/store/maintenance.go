package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/cortex-ai/cortex/services/memory-service/internal/memory"
)

// DecayResult reports one decay sweep.
type DecayResult struct {
	Scanned  int `json:"scanned"`
	Decayed  int `json:"decayed"`
	Archived int `json:"archived"`
}

// ApplyDecay recomputes salience from time since last reinforcement and
// archives what has fallen through the floor.
//
// Decay is exponential on a per-kind half-life, and it archives rather than
// deletes: an archived memory drops out of default search but is still there
// for an as-of query and still cited by the answers that used it. Deleting
// would break provenance for answers already given, which is a worse outcome
// than keeping a cold row.
//
// Decisions and instructions are exempt from archival entirely. A decision
// nobody has mentioned in two years is not a decision that stopped applying —
// it is usually one so settled that nobody argues about it anymore, and that is
// exactly the institutional memory this layer exists to hold.
func (s *Store) ApplyDecay(ctx context.Context, tenantID string, now time.Time, floor float64) (DecayResult, error) {
	var res DecayResult
	if floor <= 0 {
		floor = 0.15
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, salience, last_seen_at
		  FROM memories
		 WHERE tenant_id = $1 AND archived = FALSE AND valid_to IS NULL`, tenantID)
	if err != nil {
		return res, fmt.Errorf("store: decay scan: %w", err)
	}

	type update struct {
		id       string
		salience float64
		archive  bool
	}
	var updates []update
	for rows.Next() {
		var (
			id       string
			kindStr  string
			salience float64
			lastSeen time.Time
		)
		if err := rows.Scan(&id, &kindStr, &salience, &lastSeen); err != nil {
			rows.Close()
			return res, fmt.Errorf("store: decay scan row: %w", err)
		}
		res.Scanned++

		kind := memory.Kind(kindStr)
		elapsed := now.Sub(lastSeen)
		if elapsed <= 0 {
			continue
		}
		decayed := salience * math.Pow(0.5, elapsed.Hours()/kind.HalfLife().Hours())
		if math.Abs(decayed-salience) < 0.001 {
			continue
		}
		archive := decayed < floor && kind != memory.KindDecision && kind != memory.KindInstruction
		updates = append(updates, update{id: id, salience: decayed, archive: archive})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, fmt.Errorf("store: decay iterate: %w", err)
	}

	for _, u := range updates {
		if _, err := s.pool.Exec(ctx, `
			UPDATE memories SET salience = $2, archived = $3, updated_at = now() WHERE id = $1`,
			u.id, u.salience, u.archive); err != nil {
			return res, fmt.Errorf("store: decay update: %w", err)
		}
		res.Decayed++
		if u.archive {
			res.Archived++
		}
	}
	return res, nil
}

// ConsolidationGroup is a set of current memories on one topic that have
// accumulated past the point of being individually useful.
type ConsolidationGroup struct {
	TenantID string
	Kind     memory.Kind
	Topic    string
	Memories []memory.Memory
}

// FindConsolidationGroups returns topics carrying more than minSize current
// memories.
//
// Events are the usual source: they never supersede, so a busy topic
// accumulates dozens of individually thin records ("deploy failed at 14:02",
// "deploy failed at 14:09"). Consolidation folds those into one memory that
// says what actually happened, with the originals kept as history.
func (s *Store) FindConsolidationGroups(ctx context.Context, tenantID string, minSize int, limit int) ([]ConsolidationGroup, error) {
	if minSize < 2 {
		minSize = 5
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	rows, err := s.pool.Query(ctx, `
		SELECT kind, topic
		  FROM memories
		 WHERE tenant_id = $1 AND valid_to IS NULL AND archived = FALSE
		 GROUP BY kind, topic
		HAVING count(*) >= $2
		 ORDER BY count(*) DESC
		 LIMIT $3`, tenantID, minSize, limit)
	if err != nil {
		return nil, fmt.Errorf("store: consolidation groups: %w", err)
	}
	type key struct {
		kind  memory.Kind
		topic string
	}
	var keys []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.kind, &k.topic); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan consolidation group: %w", err)
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate consolidation groups: %w", err)
	}

	out := make([]ConsolidationGroup, 0, len(keys))
	for _, k := range keys {
		mems, err := s.Search(ctx, SearchFilter{
			TenantID: tenantID,
			Kinds:    []memory.Kind{k.kind},
			Topic:    k.topic,
			SkipACL:  true,
			Limit:    100,
		})
		if err != nil {
			return nil, err
		}
		if len(mems) < minSize {
			continue
		}
		out = append(out, ConsolidationGroup{TenantID: tenantID, Kind: k.kind, Topic: k.topic, Memories: mems})
	}
	return out, nil
}

// Consolidate replaces a group of memories with one summary memory, inheriting
// the union of their provenance, ACLs and subjects.
//
// The ACL union is the subtle part and it is deliberately conservative in the
// wrong-looking direction: the consolidated memory is visible to anyone who
// could see *any* of its inputs. That would be a leak if the summary carried
// content from a source the reader cannot access, so consolidation only ever
// groups memories that already share at least one ACL group — enforced here
// rather than trusted to the caller.
func (s *Store) Consolidate(ctx context.Context, group ConsolidationGroup, summary memory.Memory) (string, error) {
	if len(group.Memories) < 2 {
		return "", fmt.Errorf("store: consolidate: need at least 2 memories, got %d", len(group.Memories))
	}
	shared := sharedACLGroups(group.Memories)
	if len(shared) == 0 {
		return "", fmt.Errorf("store: consolidate: memories on topic %q share no ACL group; refusing to merge across permission boundaries", group.Topic)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	summary.TenantID = group.TenantID
	summary.Kind = group.Kind
	summary.Topic = group.Topic
	summary.ACLGroupIDs = shared
	if summary.IngestedAt.IsZero() {
		summary.IngestedAt = time.Now().UTC()
	}
	if summary.ValidFrom.IsZero() {
		summary.ValidFrom = group.Memories[0].ValidFrom
	}
	for _, m := range group.Memories {
		summary.Subjects = mergeStrings(summary.Subjects, m.Subjects)
		if m.ValidFrom.Before(summary.ValidFrom) {
			summary.ValidFrom = m.ValidFrom
		}
	}

	// Close the originals first: the unique index permits only one current
	// memory per topic key, so inserting the summary before retiring its inputs
	// would deadlock against its own constraint.
	closedAt := time.Now().UTC()
	ids := make([]string, 0, len(group.Memories))
	for _, m := range group.Memories {
		ids = append(ids, m.ID)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE memories SET valid_to = $2, updated_at = now()
		 WHERE id = ANY($1::uuid[]) AND valid_to IS NULL`, ids, closedAt); err != nil {
		return "", fmt.Errorf("store: close consolidated memories: %w", err)
	}

	attrs, err := json.Marshal(nonNilAttrs(summary.Attributes))
	if err != nil {
		return "", fmt.Errorf("store: marshal attributes: %w", err)
	}
	var newID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO memories (
			tenant_id, kind, topic, topic_raw, statement, attributes, subjects,
			confidence, salience, valid_from, ingested_at, acl_group_ids, last_seen_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING id`,
		summary.TenantID, string(summary.Kind), summary.Topic, summary.TopicRaw, summary.Statement,
		attrs, nonNilStrings(summary.Subjects), summary.Confidence, 1.0, summary.ValidFrom,
		summary.IngestedAt, shared, closedAt).Scan(&newID); err != nil {
		return "", fmt.Errorf("store: insert consolidated memory: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE memories SET superseded_by = $2 WHERE id = ANY($1::uuid[])`, ids, newID); err != nil {
		return "", fmt.Errorf("store: link consolidated memories: %w", err)
	}

	// Provenance is inherited wholesale — the summary must cite everything its
	// inputs cited, or consolidation would quietly destroy citability.
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_provenance (memory_id, episode_id, quote, pass)
		SELECT $1, p.episode_id, p.quote, 'consolidated'
		  FROM memory_provenance p
		 WHERE p.memory_id = ANY($2::uuid[])
		ON CONFLICT (memory_id, episode_id) DO NOTHING`, newID, ids); err != nil {
		return "", fmt.Errorf("store: inherit provenance: %w", err)
	}

	for _, id := range ids {
		if err := s.recordInvalidation(ctx, tx, group.TenantID, id, newID, closedAt, "consolidated into a summary memory"); err != nil {
			return "", err
		}
		if err := s.graph.LinkSupersedes(ctx, tx, newID, id); err != nil {
			return "", err
		}
	}

	summary.ID = newID
	if err := s.graph.UpsertMemory(ctx, tx, summary); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("store: commit consolidation: %w", err)
	}
	return newID, nil
}

// sharedACLGroups returns the groups present on every memory in the set.
func sharedACLGroups(mems []memory.Memory) []string {
	if len(mems) == 0 {
		return nil
	}
	shared := map[string]struct{}{}
	for _, g := range mems[0].ACLGroupIDs {
		shared[g] = struct{}{}
	}
	for _, m := range mems[1:] {
		have := make(map[string]struct{}, len(m.ACLGroupIDs))
		for _, g := range m.ACLGroupIDs {
			have[g] = struct{}{}
		}
		for g := range shared {
			if _, ok := have[g]; !ok {
				delete(shared, g)
			}
		}
	}
	out := make([]string, 0, len(shared))
	for g := range shared {
		out = append(out, g)
	}
	return mergeStrings(out, nil)
}

// Tenants lists tenants with memories, so the maintenance jobs can iterate
// without a dependency on the control plane's tenant table.
func (s *Store) Tenants(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT tenant_id FROM memories`)
	if err != nil {
		return nil, fmt.Errorf("store: list tenants: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("store: scan tenant: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Stats backs the admin console's memory panel and the service's own metrics.
type Stats struct {
	Total      int64            `json:"total"`
	Current    int64            `json:"current"`
	Archived   int64            `json:"archived"`
	ByKind     map[string]int64 `json:"by_kind"`
	Rejections map[string]int64 `json:"rejections_by_check"`
}

// Stats aggregates counts for one tenant.
func (s *Store) Stats(ctx context.Context, tenantID string) (Stats, error) {
	st := Stats{ByKind: map[string]int64{}, Rejections: map[string]int64{}}

	if err := s.pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE valid_to IS NULL AND archived = FALSE),
		       count(*) FILTER (WHERE archived)
		  FROM memories WHERE tenant_id = $1`, tenantID).
		Scan(&st.Total, &st.Current, &st.Archived); err != nil {
		return st, fmt.Errorf("store: stats: %w", err)
	}

	rows, err := s.pool.Query(ctx,
		`SELECT kind, count(*) FROM memories WHERE tenant_id = $1 GROUP BY kind`, tenantID)
	if err != nil {
		return st, fmt.Errorf("store: stats by kind: %w", err)
	}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			rows.Close()
			return st, fmt.Errorf("store: scan stats: %w", err)
		}
		st.ByKind[k] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, fmt.Errorf("store: iterate stats: %w", err)
	}

	// The rejection mix is the diagnostic that matters most here: a spike in
	// one check means the prompt regressed, a spike across all of them means
	// the model did.
	rejRows, err := s.pool.Query(ctx,
		`SELECT failed_check, count(*) FROM memory_rejections WHERE tenant_id = $1 GROUP BY failed_check`, tenantID)
	if err != nil {
		return st, fmt.Errorf("store: stats rejections: %w", err)
	}
	defer rejRows.Close()
	for rejRows.Next() {
		var c string
		var n int64
		if err := rejRows.Scan(&c, &n); err != nil {
			return st, fmt.Errorf("store: scan rejection stats: %w", err)
		}
		st.Rejections[c] = n
	}
	return st, rejRows.Err()
}
