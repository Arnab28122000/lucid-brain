-- Cortex memory layer — bi-temporal store, provenance index and expertise edges.
--
-- Postgres is the system of record. Apache AGE (migration 0002) projects the
-- same rows into a property graph for traversal queries; the graph is a
-- derivative, never the source of truth, so a graph rebuild is always possible
-- from these tables alone.

BEGIN;

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ---------------------------------------------------------------------------
-- Episodes: the provenance anchors. One row per distinct piece of source
-- content, keyed by content hash so stream replay is idempotent.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS memory_episodes (
    id            TEXT PRIMARY KEY,             -- <tenant>:<content_hash>
    tenant_id     TEXT        NOT NULL,
    document_id   TEXT        NOT NULL,
    content_hash  TEXT        NOT NULL,
    source_type   TEXT        NOT NULL,
    source_id     TEXT        NOT NULL DEFAULT '',
    permalink     TEXT        NOT NULL DEFAULT '',
    title         TEXT        NOT NULL DEFAULT '',
    body          TEXT        NOT NULL,
    author_key    TEXT        NOT NULL DEFAULT '',
    author_name   TEXT        NOT NULL DEFAULT '',
    participants  JSONB       NOT NULL DEFAULT '[]'::jsonb,
    acl_group_ids TEXT[]      NOT NULL DEFAULT '{}',
    occurred_at   TIMESTAMPTZ NOT NULL,
    ingested_at   TIMESTAMPTZ NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, content_hash)
);

CREATE INDEX IF NOT EXISTS memory_episodes_tenant_document_idx
    ON memory_episodes (tenant_id, document_id);
CREATE INDEX IF NOT EXISTS memory_episodes_occurred_idx
    ON memory_episodes (tenant_id, occurred_at DESC);

-- ---------------------------------------------------------------------------
-- Memories: bi-temporal. valid_from/valid_to are world time (when the statement
-- was true); ingested_at is system time (when Cortex learned it). Superseding a
-- memory sets valid_to and superseded_by -- it never deletes, because a
-- decisions timeline is only possible if what was replaced is still readable.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS memories (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     TEXT        NOT NULL,
    kind          TEXT        NOT NULL CHECK (kind IN ('fact','event','instruction','task','decision')),
    topic         TEXT        NOT NULL,          -- normalized supersession key
    topic_raw     TEXT        NOT NULL DEFAULT '',
    statement     TEXT        NOT NULL,
    attributes    JSONB       NOT NULL DEFAULT '{}'::jsonb,
    subjects      TEXT[]      NOT NULL DEFAULT '{}',
    task_status   TEXT        NULL CHECK (task_status IS NULL OR task_status IN ('open','done','dropped')),
    confidence    DOUBLE PRECISION NOT NULL DEFAULT 0.5 CHECK (confidence >= 0 AND confidence <= 1),
    salience      DOUBLE PRECISION NOT NULL DEFAULT 1.0 CHECK (salience >= 0),
    valid_from    TIMESTAMPTZ NOT NULL,
    valid_to      TIMESTAMPTZ NULL,
    ingested_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    superseded_by UUID        NULL REFERENCES memories(id) ON DELETE SET NULL,
    acl_group_ids TEXT[]      NOT NULL DEFAULT '{}',
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    archived      BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- An open interval must be genuinely open, and a closed one must not run
    -- backwards. Bad intervals silently corrupt every as-of query, so the
    -- constraint lives in the database rather than only in the resolver.
    CONSTRAINT memories_interval_ck CHECK (valid_to IS NULL OR valid_to >= valid_from),
    CONSTRAINT memories_supersede_ck CHECK ((superseded_by IS NULL) OR (valid_to IS NOT NULL))
);

-- One current memory per (tenant, kind, topic). This is the invariant that makes
-- "what is true now" a lookup rather than a ranking problem; the conflict
-- resolver is what keeps it satisfiable. Events are exempt -- they are a stream,
-- not a state, and many events legitimately share a topic.
CREATE UNIQUE INDEX IF NOT EXISTS memories_current_topic_uidx
    ON memories (tenant_id, kind, topic)
    WHERE valid_to IS NULL AND archived = FALSE AND kind <> 'event';

CREATE INDEX IF NOT EXISTS memories_topic_idx      ON memories (tenant_id, topic);
CREATE INDEX IF NOT EXISTS memories_kind_idx       ON memories (tenant_id, kind, valid_from DESC);
CREATE INDEX IF NOT EXISTS memories_valid_idx      ON memories (tenant_id, valid_from, valid_to);
CREATE INDEX IF NOT EXISTS memories_superseded_idx ON memories (superseded_by) WHERE superseded_by IS NOT NULL;
CREATE INDEX IF NOT EXISTS memories_acl_idx        ON memories USING GIN (acl_group_ids);
CREATE INDEX IF NOT EXISTS memories_subjects_idx   ON memories USING GIN (subjects);
-- Search covers statement and topic together: a query phrased the way a person
-- asks it often shares its connecting term with the topic rather than with the
-- statement that answers it. The index must match the expression in query.go
-- exactly or it will be silently unused.
CREATE INDEX IF NOT EXISTS memories_statement_fts_idx
    ON memories USING GIN (to_tsvector('english', statement || ' ' || topic_raw));
CREATE INDEX IF NOT EXISTS memories_decay_idx
    ON memories (tenant_id, last_seen_at) WHERE archived = FALSE;

-- ---------------------------------------------------------------------------
-- Provenance: the bidirectional memory <-> episode index. Indexed both ways
-- because both directions are asked: "cite this memory" walks one way,
-- "invalidate everything derived from this deleted document" walks the other.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS memory_provenance (
    memory_id  UUID NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    episode_id TEXT NOT NULL REFERENCES memory_episodes(id) ON DELETE CASCADE,
    quote      TEXT NOT NULL DEFAULT '',
    pass       TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (memory_id, episode_id)
);

CREATE INDEX IF NOT EXISTS memory_provenance_episode_idx
    ON memory_provenance (episode_id);

-- ---------------------------------------------------------------------------
-- Invalidations: an append-only audit of every edge invalidation. The memories
-- table holds the current pointers; this holds why and when they moved.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS memory_invalidations (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id      TEXT        NOT NULL,
    memory_id      UUID        NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    superseded_by  UUID        NULL REFERENCES memories(id) ON DELETE SET NULL,
    reason         TEXT        NOT NULL,
    valid_to       TIMESTAMPTZ NOT NULL,
    invalidated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS memory_invalidations_memory_idx
    ON memory_invalidations (memory_id);

-- ---------------------------------------------------------------------------
-- Rejections: candidates the verifier threw out. Kept because "why is this not
-- in the answer?" is a support question, and because the rejection mix is the
-- signal for whether to fix the prompt, the chunker or the model.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS memory_rejections (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id    TEXT        NOT NULL,
    episode_id   TEXT        NOT NULL,
    kind         TEXT        NOT NULL DEFAULT '',
    topic        TEXT        NOT NULL DEFAULT '',
    statement    TEXT        NOT NULL DEFAULT '',
    failed_check TEXT        NOT NULL,
    reason       TEXT        NOT NULL DEFAULT '',
    checks       JSONB       NOT NULL DEFAULT '[]'::jsonb,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS memory_rejections_tenant_idx
    ON memory_rejections (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS memory_rejections_check_idx
    ON memory_rejections (failed_check, created_at DESC);

-- ---------------------------------------------------------------------------
-- Expertise: person -> topic edges accumulated from authored and subject
-- memories. This is what answers "who knows about X".
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS memory_expertise (
    tenant_id    TEXT NOT NULL,
    person_key   TEXT NOT NULL,
    topic        TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    score        DOUBLE PRECISION NOT NULL DEFAULT 0,
    signals      INTEGER NOT NULL DEFAULT 0,
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, person_key, topic)
);

CREATE INDEX IF NOT EXISTS memory_expertise_topic_idx
    ON memory_expertise (tenant_id, topic, score DESC);

-- ---------------------------------------------------------------------------
-- Ingest log: consumer-side idempotency. JetStream guarantees at-least-once, so
-- the same content hash will arrive twice; extraction is expensive and
-- supersession is destructive, so the second delivery must be a no-op.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS memory_ingest_log (
    tenant_id      TEXT NOT NULL,
    content_hash   TEXT NOT NULL,
    episode_id     TEXT NOT NULL,
    memories_added INTEGER NOT NULL DEFAULT 0,
    rejected       INTEGER NOT NULL DEFAULT 0,
    processed_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, content_hash)
);

COMMIT;
