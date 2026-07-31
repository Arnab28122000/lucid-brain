// Package store is the bi-temporal memory store: Postgres for the records and
// Apache AGE for the graph projection, written in the same transaction so the
// two cannot drift.
package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cortex-ai/cortex/services/memory-service/internal/memory"
)

// Store owns the connection pool and the graph projection.
type Store struct {
	pool  *pgxpool.Pool
	graph Graph
}

// Options configures the store.
type Options struct {
	DSN string
	// GraphEnabled turns on the AGE projection. It is a flag because AGE is not
	// installed on every managed Postgres, and the relational tables answer
	// every v1 query on their own — the graph earns its keep on multi-hop
	// traversals only.
	GraphEnabled bool
	MaxConns     int32
}

// Open connects, verifies reachability, and prepares each connection for AGE.
func Open(ctx context.Context, opts Options) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("store: parse dsn: %w", err)
	}
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	if opts.GraphEnabled {
		// AGE requires LOAD and a search_path per session, not per statement.
		// Doing it in AfterConnect means every pooled connection is ready and
		// no query has to remember.
		cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			if _, err := conn.Exec(ctx, `LOAD 'age'`); err != nil {
				return fmt.Errorf("store: load age: %w", err)
			}
			if _, err := conn.Exec(ctx, `SET search_path = ag_catalog, "$user", public`); err != nil {
				return fmt.Errorf("store: age search_path: %w", err)
			}
			return nil
		}
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}

	s := &Store{pool: pool, graph: NoopGraph{}}
	if opts.GraphEnabled {
		s.graph = AGEGraph{GraphName: "cortex_memory"}
	}
	return s, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Pool exposes the pool for health checks and the migration runner.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Ping reports database reachability for /readyz.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// ErrNotFound is returned by single-row lookups.
var ErrNotFound = errors.New("store: not found")

// AlreadyProcessed reports whether this exact content has already been through
// extraction. JetStream is at-least-once, extraction costs an LLM call, and
// supersession is destructive — so the second delivery of the same content hash
// must do nothing at all.
func (s *Store) AlreadyProcessed(ctx context.Context, tenantID, contentHash string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM memory_ingest_log WHERE tenant_id = $1 AND content_hash = $2)`,
		tenantID, contentHash).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: ingest log lookup: %w", err)
	}
	return exists, nil
}

// SaveEpisode upserts the provenance anchor. Re-ingestion of edited content
// (same document, new hash) creates a new episode; the old one stays, because
// memories already cite it.
func (s *Store) SaveEpisode(ctx context.Context, tx pgx.Tx, ep memory.Episode) error {
	participants, err := marshalPeople(ep.Participants)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO memory_episodes (
			id, tenant_id, document_id, content_hash, source_type, source_id,
			permalink, title, body, author_key, author_name, participants,
			acl_group_ids, occurred_at, ingested_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT (id) DO UPDATE SET
			permalink   = EXCLUDED.permalink,
			title       = EXCLUDED.title,
			acl_group_ids = EXCLUDED.acl_group_ids,
			ingested_at = EXCLUDED.ingested_at`,
		ep.ID, ep.TenantID, ep.DocumentID, ep.ContentHash, ep.SourceType, ep.SourceID,
		ep.Permalink, ep.Title, ep.Body, ep.Author.Key(), ep.Author.DisplayName, participants,
		nonNilStrings(ep.ACLGroupIDs), ep.OccurredAt, ep.IngestedAt)
	if err != nil {
		return fmt.Errorf("store: save episode: %w", err)
	}
	return nil
}

// GetEpisode loads one episode by ID.
func (s *Store) GetEpisode(ctx context.Context, id string) (memory.Episode, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, tenant_id, document_id, content_hash, source_type, source_id,
		       permalink, title, body, author_key, author_name, participants,
		       acl_group_ids, occurred_at, ingested_at
		  FROM memory_episodes WHERE id = $1`, id)

	var (
		ep         memory.Episode
		authorKey  string
		authorName string
		raw        []byte
	)
	err := row.Scan(&ep.ID, &ep.TenantID, &ep.DocumentID, &ep.ContentHash, &ep.SourceType,
		&ep.SourceID, &ep.Permalink, &ep.Title, &ep.Body, &authorKey, &authorName,
		&raw, &ep.ACLGroupIDs, &ep.OccurredAt, &ep.IngestedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return memory.Episode{}, ErrNotFound
	}
	if err != nil {
		return memory.Episode{}, fmt.Errorf("store: get episode: %w", err)
	}
	ep.Author = memory.Person{ExternalID: authorKey, DisplayName: authorName}
	ep.Participants, err = unmarshalPeople(raw)
	if err != nil {
		return memory.Episode{}, err
	}
	return ep, nil
}

// scanMemories reads a memory result set. Callers must select the columns in
// memoryColumns order.
const memoryColumns = `
	m.id, m.tenant_id, m.kind, m.topic, m.topic_raw, m.statement, m.attributes,
	m.subjects, m.task_status, m.confidence, m.salience, m.valid_from, m.valid_to,
	m.ingested_at, m.superseded_by, m.acl_group_ids, m.last_seen_at, m.archived`

func scanMemories(rows pgx.Rows) ([]memory.Memory, error) {
	defer rows.Close()
	var out []memory.Memory
	for rows.Next() {
		var (
			m          memory.Memory
			attrs      map[string]string
			taskStatus *string
			superseded *string
		)
		if err := rows.Scan(&m.ID, &m.TenantID, &m.Kind, &m.Topic, &m.TopicRaw, &m.Statement,
			&attrs, &m.Subjects, &taskStatus, &m.Confidence, &m.Salience, &m.ValidFrom,
			&m.ValidTo, &m.IngestedAt, &superseded, &m.ACLGroupIDs, &m.LastSeenAt, &m.Archived); err != nil {
			return nil, fmt.Errorf("store: scan memory: %w", err)
		}
		m.Attributes = attrs
		if taskStatus != nil {
			m.TaskStatus = memory.TaskStatus(*taskStatus)
		}
		m.SupersededBy = superseded
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate memories: %w", err)
	}
	return out, nil
}

// attachSources fills in the provenance every memory-backed answer needs to
// cite. Done as one batched query rather than per memory — the N+1 here would
// be paid on every search result page.
func (s *Store) attachSources(ctx context.Context, mems []memory.Memory) error {
	if len(mems) == 0 {
		return nil
	}
	ids := make([]string, 0, len(mems))
	for _, m := range mems {
		ids = append(ids, m.ID)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT p.memory_id, e.id, e.source_type, e.permalink, e.title, e.occurred_at, p.quote
		  FROM memory_provenance p
		  JOIN memory_episodes e ON e.id = p.episode_id
		 WHERE p.memory_id = ANY($1::uuid[])
		 ORDER BY e.occurred_at DESC`, ids)
	if err != nil {
		return fmt.Errorf("store: load provenance: %w", err)
	}
	defer rows.Close()

	byMemory := make(map[string][]memory.SourceRef, len(mems))
	for rows.Next() {
		var (
			memID string
			ref   memory.SourceRef
		)
		if err := rows.Scan(&memID, &ref.EpisodeID, &ref.SourceType, &ref.Permalink,
			&ref.Title, &ref.OccurredAt, &ref.Quote); err != nil {
			return fmt.Errorf("store: scan provenance: %w", err)
		}
		byMemory[memID] = append(byMemory[memID], ref)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: iterate provenance: %w", err)
	}
	for i := range mems {
		mems[i].Sources = byMemory[mems[i].ID]
		mems[i].EpisodeIDs = episodeIDs(mems[i].Sources)
	}
	return nil
}

func episodeIDs(refs []memory.SourceRef) []string {
	if len(refs) == 0 {
		return nil
	}
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.EpisodeID)
	}
	return out
}

// --- small helpers ---

func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// mergeStrings unions two string slices, preserving order and dropping blanks.
func mergeStrings(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range append(append([]string{}, a...), b...) {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func timePtr(t time.Time) *time.Time { return &t }
