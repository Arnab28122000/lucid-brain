package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cortex-ai/cortex/services/memory-service/internal/memory"
)

// Proposal is a verified candidate on its way into the store, paired with the
// quote that grounds it. The store deliberately does not import the extractor:
// what it needs is a memory and its evidence, not a pipeline stage's type.
type Proposal struct {
	Memory memory.Memory
	Quote  string
	Pass   string
}

// RejectionRecord is a verifier rejection, persisted for the extraction
// inspector and for prompt-regression analysis.
type RejectionRecord struct {
	Kind        string
	Topic       string
	Statement   string
	FailedCheck string
	Reason      string
	Checks      any
}

// Outcome describes what happened to one proposal. Callers emit these as
// metrics — the ratio of superseded to created is the single best indicator of
// whether topic normalization is too loose.
type Outcome string

const (
	// OutcomeCreated is a new memory on a topic that had none.
	OutcomeCreated Outcome = "created"
	// OutcomeReinforced is the same assertion seen again: provenance is
	// extended and salience bumped, but no new row is written.
	OutcomeReinforced Outcome = "reinforced"
	// OutcomeSuperseded is a newer assertion invalidating the current one.
	OutcomeSuperseded Outcome = "superseded"
	// OutcomeBackfilled is an *older* assertion arriving after a newer one —
	// it is filed into history without disturbing what is currently true.
	OutcomeBackfilled Outcome = "backfilled"
)

// CommitResult reports the effect of one episode.
type CommitResult struct {
	EpisodeID  string
	MemoryIDs  []string
	Outcomes   map[Outcome]int
	Rejected   int
	Superseded []Supersession
}

// Supersession records one edge invalidation, for the decisions timeline and
// for the audit log.
type Supersession struct {
	OldID   string    `json:"old_id"`
	NewID   string    `json:"new_id"`
	Topic   string    `json:"topic"`
	Kind    string    `json:"kind"`
	ValidTo time.Time `json:"valid_to"`
	Reason  string    `json:"reason"`
}

// reinforcementBump is how much salience a re-observation adds. Small on
// purpose: memories should be kept alive by being *used*, and repeated
// mentions of the same thing in a chatty channel are not the same as
// importance.
const (
	reinforcementBump = 0.25
	maxSalience       = 2.0
)

// Commit writes one episode's verified memories, resolving conflicts against
// what is already current. Everything lands in a single transaction: a partial
// commit could leave a topic with two current memories or with none, and both
// are worse than reprocessing the episode.
func (s *Store) Commit(ctx context.Context, ep memory.Episode, proposals []Proposal, rejections []RejectionRecord) (CommitResult, error) {
	res := CommitResult{EpisodeID: ep.ID, Outcomes: map[Outcome]int{}, Rejected: len(rejections)}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.SaveEpisode(ctx, tx, ep); err != nil {
		return res, err
	}
	if err := s.graph.UpsertEpisode(ctx, tx, ep); err != nil {
		return res, err
	}

	for _, p := range proposals {
		outcome, id, sup, err := s.commitOne(ctx, tx, ep, p)
		if err != nil {
			return res, err
		}
		res.Outcomes[outcome]++
		res.MemoryIDs = append(res.MemoryIDs, id)
		if sup != nil {
			res.Superseded = append(res.Superseded, *sup)
		}
	}

	if err := s.recordExpertise(ctx, tx, ep, proposals); err != nil {
		return res, err
	}
	if err := s.recordRejections(ctx, tx, ep, rejections); err != nil {
		return res, err
	}

	// The ingest log is written last and inside the same transaction: if
	// anything above fails, the episode is not marked processed and redelivery
	// will retry it. Marking it processed first would turn a transient LLM
	// failure into permanent data loss.
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_ingest_log (tenant_id, content_hash, episode_id, memories_added, rejected)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (tenant_id, content_hash) DO UPDATE SET
			memories_added = memory_ingest_log.memories_added + EXCLUDED.memories_added,
			rejected       = memory_ingest_log.rejected + EXCLUDED.rejected,
			processed_at   = now()`,
		ep.TenantID, ep.ContentHash, ep.ID, len(res.MemoryIDs), len(rejections)); err != nil {
		return res, fmt.Errorf("store: ingest log: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("store: commit: %w", err)
	}
	return res, nil
}

func (s *Store) commitOne(ctx context.Context, tx pgx.Tx, ep memory.Episode, p Proposal) (Outcome, string, *Supersession, error) {
	m := p.Memory

	// Events are a stream, not a state: two things that happened do not
	// contradict each other, so they never take the supersession path.
	if !m.Kind.Supersedes() {
		id, err := s.insertMemory(ctx, tx, m, nil)
		if err != nil {
			return "", "", nil, err
		}
		if err := s.linkProvenance(ctx, tx, id, ep, p); err != nil {
			return "", "", nil, err
		}
		return OutcomeCreated, id, nil, nil
	}

	current, err := s.lockCurrent(ctx, tx, m.TenantID, m.Kind, m.Topic)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return "", "", nil, err
	}

	if errors.Is(err, ErrNotFound) {
		id, err := s.insertMemory(ctx, tx, m, nil)
		if err != nil {
			return "", "", nil, err
		}
		if err := s.linkProvenance(ctx, tx, id, ep, p); err != nil {
			return "", "", nil, err
		}
		return OutcomeCreated, id, nil, nil
	}

	// Same topic, same claim: this is corroboration, not conflict. Writing a
	// new row would inflate the supersession chain with an "update" that
	// changed nothing and would make the decisions timeline unreadable.
	if isRestatement(current, m) {
		if err := s.reinforce(ctx, tx, current, m, ep, p); err != nil {
			return "", "", nil, err
		}
		return OutcomeReinforced, current.ID, nil, nil
	}

	// The new memory describes an *earlier* world state than what is current —
	// a backfilled thread, or a connector catching up out of order. It belongs
	// in history, closed off at the point the current memory took over. The
	// current memory is left alone.
	if m.ValidFrom.Before(current.ValidFrom) {
		historical := m
		historical.ValidTo = timePtr(current.ValidFrom)
		id, err := s.insertMemory(ctx, tx, historical, &current.ID)
		if err != nil {
			return "", "", nil, err
		}
		if err := s.linkProvenance(ctx, tx, id, ep, p); err != nil {
			return "", "", nil, err
		}
		if err := s.recordInvalidation(ctx, tx, m.TenantID, id, current.ID, current.ValidFrom,
			"backfilled memory closed off by the newer memory already current"); err != nil {
			return "", "", nil, err
		}
		if err := s.graph.LinkSupersedes(ctx, tx, current.ID, id); err != nil {
			return "", "", nil, err
		}
		return OutcomeBackfilled, id, nil, nil
	}

	// The ordinary case, and the one the worked example turns on: a Slack
	// decision arrives on a topic a Confluence page already claimed. The old
	// memory's world-validity ends where the new one begins, and it is linked
	// forward rather than deleted — so the answer can still show what it
	// replaced and when.
	//
	// Closing the old memory has to happen *before* the new one is inserted:
	// the partial unique index permits one open-ended memory per topic key, so
	// the reverse order collides with the very invariant this branch exists to
	// maintain. The forward link is then set in a second update, since the new
	// ID does not exist until the insert returns.
	validTo := m.ValidFrom
	reason := fmt.Sprintf("superseded by newer %s on topic %q from %s", m.Kind, m.Topic, ep.SourceType)
	if _, err := tx.Exec(ctx, `
		UPDATE memories SET valid_to = $2, updated_at = now() WHERE id = $1`,
		current.ID, validTo); err != nil {
		return "", "", nil, fmt.Errorf("store: close superseded memory: %w", err)
	}

	id, err := s.insertMemory(ctx, tx, m, nil)
	if err != nil {
		return "", "", nil, err
	}
	if err := s.linkProvenance(ctx, tx, id, ep, p); err != nil {
		return "", "", nil, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE memories SET superseded_by = $2, updated_at = now() WHERE id = $1`,
		current.ID, id); err != nil {
		return "", "", nil, fmt.Errorf("store: link superseded memory: %w", err)
	}
	if err := s.recordInvalidation(ctx, tx, m.TenantID, current.ID, id, validTo, reason); err != nil {
		return "", "", nil, err
	}
	if err := s.graph.LinkSupersedes(ctx, tx, id, current.ID); err != nil {
		return "", "", nil, err
	}

	return OutcomeSuperseded, id, &Supersession{
		OldID:   current.ID,
		NewID:   id,
		Topic:   m.Topic,
		Kind:    string(m.Kind),
		ValidTo: validTo,
		Reason:  reason,
	}, nil
}

// lockCurrent takes a row lock on the current memory for a topic key. The lock
// is what makes concurrent consumers safe: two workers extracting two messages
// from the same thread will otherwise both read "no current memory" and both
// insert, and only the unique index will stop them — as an error, after the LLM
// spend.
func (s *Store) lockCurrent(ctx context.Context, tx pgx.Tx, tenantID string, kind memory.Kind, topic string) (memory.Memory, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+memoryColumns+`
		  FROM memories m
		 WHERE m.tenant_id = $1 AND m.kind = $2 AND m.topic = $3
		   AND m.valid_to IS NULL AND m.archived = FALSE
		 FOR UPDATE`, tenantID, string(kind), topic)
	if err != nil {
		return memory.Memory{}, fmt.Errorf("store: lock current: %w", err)
	}
	mems, err := scanMemories(rows)
	if err != nil {
		return memory.Memory{}, err
	}
	if len(mems) == 0 {
		return memory.Memory{}, ErrNotFound
	}
	return mems[0], nil
}

func (s *Store) insertMemory(ctx context.Context, tx pgx.Tx, m memory.Memory, supersededBy *string) (string, error) {
	attrs, err := json.Marshal(nonNilAttrs(m.Attributes))
	if err != nil {
		return "", fmt.Errorf("store: marshal attributes: %w", err)
	}
	var taskStatus *string
	if m.TaskStatus != "" {
		ts := string(m.TaskStatus)
		taskStatus = &ts
	}
	salience := m.Salience
	if salience == 0 {
		salience = 1
	}
	lastSeen := m.LastSeenAt
	if lastSeen.IsZero() {
		lastSeen = m.IngestedAt
	}

	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO memories (
			tenant_id, kind, topic, topic_raw, statement, attributes, subjects,
			task_status, confidence, salience, valid_from, valid_to, ingested_at,
			superseded_by, acl_group_ids, last_seen_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		RETURNING id`,
		m.TenantID, string(m.Kind), m.Topic, m.TopicRaw, m.Statement, attrs,
		nonNilStrings(m.Subjects), taskStatus, m.Confidence, salience, m.ValidFrom,
		m.ValidTo, m.IngestedAt, supersededBy, nonNilStrings(m.ACLGroupIDs), lastSeen).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: insert memory: %w", err)
	}

	m.ID = id
	if err := s.graph.UpsertMemory(ctx, tx, m); err != nil {
		return "", err
	}
	return id, nil
}

// reinforce extends an existing memory rather than replacing it: new
// provenance, unioned ACLs and attributes, a salience bump, and the later of
// the two last-seen times.
func (s *Store) reinforce(ctx context.Context, tx pgx.Tx, current, incoming memory.Memory, ep memory.Episode, p Proposal) error {
	attrs, err := json.Marshal(mergeAttrs(current.Attributes, incoming.Attributes))
	if err != nil {
		return fmt.Errorf("store: marshal attributes: %w", err)
	}
	confidence := current.Confidence
	if incoming.Confidence > confidence {
		confidence = incoming.Confidence
	}
	if _, err := tx.Exec(ctx, `
		UPDATE memories
		   SET salience      = LEAST($2, salience + $3),
		       confidence    = $4,
		       attributes    = $5,
		       subjects      = $6,
		       acl_group_ids = $7,
		       last_seen_at  = GREATEST(last_seen_at, $8),
		       archived      = FALSE,
		       updated_at    = now()
		 WHERE id = $1`,
		current.ID, maxSalience, reinforcementBump, confidence, attrs,
		mergeStrings(current.Subjects, incoming.Subjects),
		mergeStrings(current.ACLGroupIDs, incoming.ACLGroupIDs),
		ep.OccurredAt); err != nil {
		return fmt.Errorf("store: reinforce memory: %w", err)
	}
	return s.linkProvenance(ctx, tx, current.ID, ep, p)
}

// linkProvenance writes both halves of the memory <-> episode index and its
// graph edge. Provenance is what makes a memory-backed answer citable rather
// than merely plausible, so it is written in the same transaction as the memory
// — never fixed up afterwards.
func (s *Store) linkProvenance(ctx context.Context, tx pgx.Tx, memoryID string, ep memory.Episode, p Proposal) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_provenance (memory_id, episode_id, quote, pass)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (memory_id, episode_id) DO UPDATE SET
			quote = CASE WHEN EXCLUDED.quote <> '' THEN EXCLUDED.quote ELSE memory_provenance.quote END`,
		memoryID, ep.ID, p.Quote, p.Pass); err != nil {
		return fmt.Errorf("store: link provenance: %w", err)
	}
	return s.graph.LinkDerivedFrom(ctx, tx, memoryID, ep.ID)
}

func (s *Store) recordInvalidation(ctx context.Context, tx pgx.Tx, tenantID, memoryID, supersededBy string, validTo time.Time, reason string) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_invalidations (tenant_id, memory_id, superseded_by, reason, valid_to)
		VALUES ($1,$2,$3,$4,$5)`, tenantID, memoryID, nullableID(supersededBy), reason, validTo); err != nil {
		return fmt.Errorf("store: record invalidation: %w", err)
	}
	return nil
}

// recordExpertise credits the episode's author and each memory's subjects with
// the topic. Authorship is a weaker signal than being named as the subject —
// posting a link about Kubernetes is not the same as being the person everyone
// asks about it.
const (
	authorWeight  = 0.6
	subjectWeight = 1.0
)

func (s *Store) recordExpertise(ctx context.Context, tx pgx.Tx, ep memory.Episode, proposals []Proposal) error {
	type edge struct {
		person string
		name   string
		topic  string
		weight float64
	}
	edges := map[string]edge{}
	add := func(personKey, name, topic string, w float64) {
		if personKey == "" || personKey == "name:" || topic == "" {
			return
		}
		k := personKey + "\x00" + topic
		e := edges[k]
		e.person, e.topic = personKey, topic
		if name != "" {
			e.name = name
		}
		e.weight += w
		edges[k] = e
	}

	names := map[string]string{ep.Author.Key(): ep.Author.DisplayName}
	for _, p := range ep.Participants {
		names[p.Key()] = p.DisplayName
	}

	for _, p := range proposals {
		add(ep.Author.Key(), ep.Author.DisplayName, p.Memory.Topic, authorWeight*p.Memory.Confidence)
		for _, subj := range p.Memory.Subjects {
			add(subj, names[subj], p.Memory.Topic, subjectWeight*p.Memory.Confidence)
		}
	}

	for _, e := range edges {
		if _, err := tx.Exec(ctx, `
			INSERT INTO memory_expertise (tenant_id, person_key, topic, display_name, score, signals, last_seen_at)
			VALUES ($1,$2,$3,$4,$5,1,$6)
			ON CONFLICT (tenant_id, person_key, topic) DO UPDATE SET
				score        = memory_expertise.score + EXCLUDED.score,
				signals      = memory_expertise.signals + 1,
				display_name = CASE WHEN EXCLUDED.display_name <> '' THEN EXCLUDED.display_name ELSE memory_expertise.display_name END,
				last_seen_at = GREATEST(memory_expertise.last_seen_at, EXCLUDED.last_seen_at)`,
			ep.TenantID, e.person, e.topic, e.name, e.weight, ep.OccurredAt); err != nil {
			return fmt.Errorf("store: record expertise: %w", err)
		}
		if err := s.graph.LinkExpertise(ctx, tx, ep.TenantID, e.person, e.name, e.topic, e.weight); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) recordRejections(ctx context.Context, tx pgx.Tx, ep memory.Episode, rejections []RejectionRecord) error {
	for _, r := range rejections {
		checks, err := json.Marshal(r.Checks)
		if err != nil {
			checks = []byte("[]")
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO memory_rejections (tenant_id, episode_id, kind, topic, statement, failed_check, reason, checks)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			ep.TenantID, ep.ID, r.Kind, r.Topic, r.Statement, r.FailedCheck, r.Reason, checks); err != nil {
			return fmt.Errorf("store: record rejection: %w", err)
		}
	}
	return nil
}

// isRestatement decides reinforcement versus supersession, and it is the most
// consequential predicate in the package. Too loose and a genuine reversal is
// swallowed as a restatement, leaving the stale memory current — the exact
// failure the temporal layer exists to prevent. Too tight and every re-mention
// writes a new row and the timeline fills with noise.
//
// The rule: the statements must be substantially the same text AND must not
// disagree on any shared attribute. The attribute check is what catches
// "pricing is $40/seat" versus "pricing is $50/seat" — near-identical
// sentences that mean opposite things.
func isRestatement(current, incoming memory.Memory) bool {
	for k, v := range incoming.Attributes {
		if cur, ok := current.Attributes[k]; ok && !equalFoldTrim(cur, v) {
			return false
		}
	}
	if incoming.Kind == memory.KindTask && current.TaskStatus != incoming.TaskStatus {
		return false // a task closing is a state change, not a restatement
	}
	return memory.StatementSimilarity(current.Statement, incoming.Statement) >= 0.8
}

func nonNilAttrs(in map[string]string) map[string]string {
	if in == nil {
		return map[string]string{}
	}
	return in
}

func mergeAttrs(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		if _, exists := out[k]; !exists {
			out[k] = v
		}
	}
	return out
}

func equalFoldTrim(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}
