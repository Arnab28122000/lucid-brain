package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cortex-ai/cortex/services/memory-service/internal/memory"
)

// SearchFilter is the read path the query gateway calls into when it assembles
// context. Memory is injected alongside retrieved chunks, so it is subject to
// the same permission parity rule: nothing comes back that the user could not
// open at the source.
type SearchFilter struct {
	TenantID string
	Query    string
	Topic    string
	Kinds    []memory.Kind
	Subjects []string

	// ACLGroupIDs is the caller's expanded group set. A memory is visible only
	// if it shares a group with the caller — the same early-binding filter the
	// vector search applies, so memory injection cannot become the leak that
	// chunk retrieval was careful to avoid.
	ACLGroupIDs []string
	// SkipACL is for internal jobs (decay, consolidation) that operate on the
	// whole tenant. It is never settable from an HTTP request.
	SkipACL bool

	// AsOf queries world time: "what did we hold true on this date". Nil means
	// now, which is the overwhelmingly common case.
	AsOf              *time.Time
	IncludeSuperseded bool
	IncludeArchived   bool
	MinConfidence     float64
	Limit             int
}

// Search returns memories ranked by textual match, salience and confidence.
//
// Ranking here is deliberately simple: memories are a small, high-precision
// corpus injected next to chunk retrieval, not a replacement for it. Spending a
// reranker pass on a few dozen rows would buy little and add a hop to the
// latency budget the retrieval plane has already spent.
func (s *Store) Search(ctx context.Context, f SearchFilter) ([]memory.Memory, error) {
	if f.TenantID == "" {
		return nil, errors.New("store: tenant_id required")
	}
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	asOf := time.Now().UTC()
	if f.AsOf != nil {
		asOf = *f.AsOf
	}

	var (
		where = []string{"m.tenant_id = $1"}
		args  = []any{f.TenantID}
	)
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	if !f.IncludeArchived {
		where = append(where, "m.archived = FALSE")
	}
	if !f.IncludeSuperseded {
		// Bi-temporal validity at the requested world time: the memory had
		// started, and had not yet been superseded.
		p := arg(asOf)
		where = append(where, fmt.Sprintf("m.valid_from <= %s AND (m.valid_to IS NULL OR m.valid_to > %s)", p, p))
	}
	if len(f.Kinds) > 0 {
		kinds := make([]string, 0, len(f.Kinds))
		for _, k := range f.Kinds {
			kinds = append(kinds, string(k))
		}
		where = append(where, "m.kind = ANY("+arg(kinds)+")")
	}
	if f.Topic != "" {
		where = append(where, "m.topic = "+arg(memory.NormalizeTopic(f.Topic)))
	}
	if len(f.Subjects) > 0 {
		where = append(where, "m.subjects && "+arg(f.Subjects))
	}
	if f.MinConfidence > 0 {
		where = append(where, "m.confidence >= "+arg(f.MinConfidence))
	}
	if !f.SkipACL {
		// An empty group set can only match memories with an empty ACL, which
		// the ingestion plane does not produce — so an unauthenticated caller
		// sees nothing rather than everything.
		where = append(where, "m.acl_group_ids && "+arg(nonNilStrings(f.ACLGroupIDs)))
	}

	rank := "0.0"
	if q := strings.TrimSpace(f.Query); q != "" {
		p := arg(q)
		// The searchable text is the statement *and* the topic. A query phrased
		// the way a person asks ("enterprise plan pricing") frequently shares no
		// stem with the statement that answers it ("the Enterprise plan moves to
		// $40 per seat per month") — the connecting word lives in the topic,
		// which is exactly what topics are for.
		vec := "to_tsvector('english', m.statement || ' ' || m.topic_raw)"
		where = append(where, fmt.Sprintf(
			"(%s @@ plainto_tsquery('english', %s) OR m.statement ILIKE '%%' || %s || '%%')", vec, p, p))
		rank = fmt.Sprintf("ts_rank(%s, plainto_tsquery('english', %s))", vec, p)
	}

	sql := fmt.Sprintf(`
		SELECT %s
		  FROM memories m
		 WHERE %s
		 ORDER BY (%s * 2.0) + (m.salience * 0.5) + (m.confidence * 0.5)
		          + (CASE WHEN m.valid_to IS NULL THEN 0.5 ELSE 0 END) DESC,
		          m.valid_from DESC
		 LIMIT %s`, memoryColumns, strings.Join(where, " AND "), rank, arg(limit))

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("store: search: %w", err)
	}
	mems, err := scanMemories(rows)
	if err != nil {
		return nil, err
	}
	if err := s.attachSources(ctx, mems); err != nil {
		return nil, err
	}
	return mems, nil
}

// Get loads one memory with its provenance and the chain of what it replaced.
func (s *Store) Get(ctx context.Context, tenantID, id string) (memory.Memory, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+memoryColumns+` FROM memories m WHERE m.tenant_id = $1 AND m.id = $2`, tenantID, id)
	if err != nil {
		return memory.Memory{}, fmt.Errorf("store: get memory: %w", err)
	}
	mems, err := scanMemories(rows)
	if err != nil {
		return memory.Memory{}, err
	}
	if len(mems) == 0 {
		return memory.Memory{}, ErrNotFound
	}
	if err := s.attachSources(ctx, mems); err != nil {
		return memory.Memory{}, err
	}
	m := mems[0]

	supersedes, err := s.supersededIDs(ctx, m.ID)
	if err != nil {
		return memory.Memory{}, err
	}
	m.Supersedes = supersedes
	return m, nil
}

func (s *Store) supersededIDs(ctx context.Context, id string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM memories WHERE superseded_by = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("store: superseded ids: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("store: scan superseded id: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// History walks a topic's full supersession chain in world-time order. This is
// the query behind "what did we believe, and when did that change".
func (s *Store) History(ctx context.Context, tenantID string, kind memory.Kind, topic string, aclGroups []string, skipACL bool) ([]memory.Memory, error) {
	where := []string{"m.tenant_id = $1", "m.kind = $2", "m.topic = $3"}
	args := []any{tenantID, string(kind), memory.NormalizeTopic(topic)}
	if !skipACL {
		args = append(args, nonNilStrings(aclGroups))
		where = append(where, fmt.Sprintf("m.acl_group_ids && $%d", len(args)))
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT %s FROM memories m
		 WHERE %s
		 ORDER BY m.valid_from ASC, m.ingested_at ASC`, memoryColumns, strings.Join(where, " AND ")), args...)
	if err != nil {
		return nil, fmt.Errorf("store: history: %w", err)
	}
	mems, err := scanMemories(rows)
	if err != nil {
		return nil, err
	}
	if err := s.attachSources(ctx, mems); err != nil {
		return nil, err
	}
	return mems, nil
}

// TimelineEntry is one row of the decisions timeline: a decision, what it
// replaced, and the sources on both sides.
type TimelineEntry struct {
	Memory    memory.Memory  `json:"memory"`
	Replaced  *memory.Memory `json:"replaced,omitempty"`
	Current   bool           `json:"current"`
	ChangedAt time.Time      `json:"changed_at"`
}

// TimelineFilter scopes the decisions timeline.
type TimelineFilter struct {
	TenantID    string
	Topic       string
	Kinds       []memory.Kind
	From        *time.Time
	To          *time.Time
	ACLGroupIDs []string
	SkipACL     bool
	Limit       int
}

// Timeline returns decisions newest-first with the memory each one replaced.
// Superseded entries are included by design — a timeline that hid them would be
// a list, and the point is to show the reversal.
func (s *Store) Timeline(ctx context.Context, f TimelineFilter) ([]TimelineEntry, error) {
	if f.TenantID == "" {
		return nil, errors.New("store: tenant_id required")
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	kinds := f.Kinds
	if len(kinds) == 0 {
		kinds = []memory.Kind{memory.KindDecision}
	}
	kindStrs := make([]string, 0, len(kinds))
	for _, k := range kinds {
		kindStrs = append(kindStrs, string(k))
	}

	where := []string{"m.tenant_id = $1", "m.kind = ANY($2)", "m.archived = FALSE"}
	args := []any{f.TenantID, kindStrs}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.Topic != "" {
		where = append(where, "m.topic = "+arg(memory.NormalizeTopic(f.Topic)))
	}
	if f.From != nil {
		where = append(where, "m.valid_from >= "+arg(*f.From))
	}
	if f.To != nil {
		where = append(where, "m.valid_from <= "+arg(*f.To))
	}
	if !f.SkipACL {
		where = append(where, "m.acl_group_ids && "+arg(nonNilStrings(f.ACLGroupIDs)))
	}

	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT %s FROM memories m
		 WHERE %s
		 ORDER BY m.valid_from DESC
		 LIMIT %s`, memoryColumns, strings.Join(where, " AND "), arg(limit)), args...)
	if err != nil {
		return nil, fmt.Errorf("store: timeline: %w", err)
	}
	mems, err := scanMemories(rows)
	if err != nil {
		return nil, err
	}
	if err := s.attachSources(ctx, mems); err != nil {
		return nil, err
	}
	if len(mems) == 0 {
		return nil, nil
	}

	// Resolve what each entry replaced in one batched query rather than one per
	// row: a timeline page is exactly the shape that makes an N+1 expensive.
	ids := make([]string, 0, len(mems))
	for _, m := range mems {
		ids = append(ids, m.ID)
	}
	replacedRows, err := s.pool.Query(ctx, `
		SELECT `+memoryColumns+`, m.superseded_by
		  FROM memories m
		 WHERE m.superseded_by = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: timeline replaced: %w", err)
	}
	replaced := map[string]memory.Memory{}
	{
		defer replacedRows.Close()
		for replacedRows.Next() {
			var (
				m          memory.Memory
				attrs      map[string]string
				taskStatus *string
				superseded *string
				byID       *string
			)
			if err := replacedRows.Scan(&m.ID, &m.TenantID, &m.Kind, &m.Topic, &m.TopicRaw, &m.Statement,
				&attrs, &m.Subjects, &taskStatus, &m.Confidence, &m.Salience, &m.ValidFrom,
				&m.ValidTo, &m.IngestedAt, &superseded, &m.ACLGroupIDs, &m.LastSeenAt, &m.Archived, &byID); err != nil {
				return nil, fmt.Errorf("store: scan replaced: %w", err)
			}
			m.Attributes = attrs
			m.SupersededBy = superseded
			if taskStatus != nil {
				m.TaskStatus = memory.TaskStatus(*taskStatus)
			}
			if byID != nil {
				replaced[*byID] = m
			}
		}
		if err := replacedRows.Err(); err != nil {
			return nil, fmt.Errorf("store: iterate replaced: %w", err)
		}
	}

	out := make([]TimelineEntry, 0, len(mems))
	for _, m := range mems {
		entry := TimelineEntry{Memory: m, Current: m.IsCurrent(), ChangedAt: m.ValidFrom}
		if r, ok := replaced[m.ID]; ok {
			rc := r
			entry.Replaced = &rc
		}
		out = append(out, entry)
	}
	return out, nil
}

// Experts ranks people for a topic. The score is accumulated at write time, so
// this is a sorted read rather than an aggregation over memories.
func (s *Store) Experts(ctx context.Context, tenantID, topic string, limit int) ([]memory.Expertise, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	normalized := memory.NormalizeTopic(topic)

	// Exact topic first, then related topics by token overlap. Expertise is
	// broader than any single normalized key — someone who owns "qdrant
	// quantization" plainly knows about "qdrant sizing" too.
	rows, err := s.pool.Query(ctx, `
		SELECT tenant_id, person_key, display_name, topic, score, signals, last_seen_at
		  FROM memory_expertise
		 WHERE tenant_id = $1
		   AND (topic = $2 OR topic LIKE '%' || $3 || '%')
		 ORDER BY (CASE WHEN topic = $2 THEN 1 ELSE 0 END) DESC, score DESC
		 LIMIT $4`, tenantID, normalized, firstToken(normalized), limit*4)
	if err != nil {
		return nil, fmt.Errorf("store: experts: %w", err)
	}
	defer rows.Close()

	// Collapse per-topic rows into one score per person, discounting related
	// topics so an exact-topic owner outranks someone adjacent to it.
	byPerson := map[string]*memory.Expertise{}
	var order []string
	for rows.Next() {
		var e memory.Expertise
		if err := rows.Scan(&e.TenantID, &e.PersonKey, &e.DisplayName, &e.Topic, &e.Score, &e.Signals, &e.LastSeenAt); err != nil {
			return nil, fmt.Errorf("store: scan expertise: %w", err)
		}
		weight := memory.TopicOverlap(normalized, e.Topic)
		if e.Topic == normalized {
			weight = 1
		}
		if weight == 0 {
			continue
		}
		agg, ok := byPerson[e.PersonKey]
		if !ok {
			cp := e
			cp.Topic = normalized
			cp.Score = 0
			cp.Signals = 0
			byPerson[e.PersonKey] = &cp
			order = append(order, e.PersonKey)
			agg = &cp
		}
		agg.Score += e.Score * weight
		agg.Signals += e.Signals
		if e.LastSeenAt.After(agg.LastSeenAt) {
			agg.LastSeenAt = e.LastSeenAt
		}
		if agg.DisplayName == "" {
			agg.DisplayName = e.DisplayName
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate expertise: %w", err)
	}

	out := make([]memory.Expertise, 0, len(order))
	for _, k := range order {
		out = append(out, *byPerson[k])
	}
	sortExpertise(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// InvalidateByDocument closes off every memory derived from a document, for
// source deletion and right-to-be-forgotten. Memories are invalidated, not
// deleted, unless purge is set — a deletion request is the one case where the
// "never delete" rule yields, because the regulation outranks the timeline.
func (s *Store) InvalidateByDocument(ctx context.Context, tenantID, documentID string, at time.Time, purge bool) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT DISTINCT p.memory_id
		  FROM memory_provenance p
		  JOIN memory_episodes e ON e.id = p.episode_id
		 WHERE e.tenant_id = $1 AND e.document_id = $2`, tenantID, documentID)
	if err != nil {
		return 0, fmt.Errorf("store: find memories for document: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: scan memory id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: iterate memory ids: %w", err)
	}
	if len(ids) == 0 {
		return 0, tx.Commit(ctx)
	}

	if purge {
		if _, err := tx.Exec(ctx, `DELETE FROM memories WHERE tenant_id = $1 AND id = ANY($2::uuid[])`, tenantID, ids); err != nil {
			return 0, fmt.Errorf("store: purge memories: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM memory_episodes WHERE tenant_id = $1 AND document_id = $2`, tenantID, documentID); err != nil {
			return 0, fmt.Errorf("store: purge episodes: %w", err)
		}
		return len(ids), tx.Commit(ctx)
	}

	// A memory corroborated by other, still-live sources keeps standing: only
	// those whose entire provenance came from this document are closed off.
	tag, err := tx.Exec(ctx, `
		UPDATE memories m
		   SET valid_to = COALESCE(m.valid_to, $3), updated_at = now()
		 WHERE m.tenant_id = $1
		   AND m.id = ANY($2::uuid[])
		   AND m.valid_to IS NULL
		   AND NOT EXISTS (
		         SELECT 1 FROM memory_provenance p
		           JOIN memory_episodes e ON e.id = p.episode_id
		          WHERE p.memory_id = m.id AND e.document_id <> $4)`,
		tenantID, ids, at, documentID)
	if err != nil {
		return 0, fmt.Errorf("store: invalidate memories: %w", err)
	}
	for _, id := range ids {
		if err := s.recordInvalidation(ctx, tx, tenantID, id, "", at, "source document deleted"); err != nil {
			return 0, err
		}
	}
	return int(tag.RowsAffected()), tx.Commit(ctx)
}

// --- people JSON helpers ---

func marshalPeople(in []memory.Person) ([]byte, error) {
	if in == nil {
		in = []memory.Person{}
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("store: marshal participants: %w", err)
	}
	return b, nil
}

func unmarshalPeople(raw []byte) ([]memory.Person, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out []memory.Person
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("store: unmarshal participants: %w", err)
	}
	return out, nil
}

func firstToken(normalized string) string {
	if normalized == "" {
		return ""
	}
	if i := strings.IndexByte(normalized, '-'); i > 0 {
		return normalized[:i]
	}
	return normalized
}

func sortExpertise(in []memory.Expertise) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j].Score > in[j-1].Score; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}

// nullableID writes SQL NULL rather than an empty string for an absent
// superseding memory — the empty string would violate the foreign key.
func nullableID(id string) any {
	if id == "" {
		return nil
	}
	return id
}
