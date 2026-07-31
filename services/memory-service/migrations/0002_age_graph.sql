-- Apache AGE projection of the memory layer.
--
-- Why a graph at all, given the tables above already answer supersession and
-- provenance with plain joins: the queries that justify it are the multi-hop
-- ones. "Who should review this decision" is person -> topic -> adjacent topics
-- -> people, and "what else did that reversal knock over" walks a supersession
-- chain of unknown depth. Both are recursive CTEs in SQL and one line of Cypher.
--
-- Why AGE rather than a second database: operating Neo4j alongside Postgres buys
-- a second backup story, a second failover story and a second permission model,
-- for a workload that is a rounding error next to Qdrant. AGE keeps it in the
-- transaction we are already in -- the graph write and the row write commit
-- together or not at all, so the projection cannot drift.
--
-- Scope discipline: this is a graph of memories, topics, people and episodes --
-- not a GraphRAG entity graph. Extracting every entity and relation on every
-- ingest is the cost that makes those systems hard to justify; this graph only
-- holds structure the relational tables already committed to.

BEGIN;

CREATE EXTENSION IF NOT EXISTS age;

LOAD 'age';
SET search_path = ag_catalog, "$user", public;

SELECT create_graph('cortex_memory')
WHERE NOT EXISTS (SELECT 1 FROM ag_graph WHERE name = 'cortex_memory');

-- Vertex labels: Memory, Topic, Person, Episode.
-- Edge labels:
--   (Memory)-[:ABOUT]->(Topic)          what the memory keys on
--   (Memory)-[:DERIVED_FROM]->(Episode) provenance, mirrors memory_provenance
--   (Memory)-[:SUPERSEDES]->(Memory)    edge invalidation, newer -> older
--   (Memory)-[:MENTIONS]->(Person)      subjects
--   (Person)-[:EXPERT_IN]->(Topic)      accumulated expertise, mirrors memory_expertise
SELECT create_vlabel('cortex_memory', 'Memory')  WHERE _label_id('cortex_memory', 'Memory')  = 0;
SELECT create_vlabel('cortex_memory', 'Topic')   WHERE _label_id('cortex_memory', 'Topic')   = 0;
SELECT create_vlabel('cortex_memory', 'Person')  WHERE _label_id('cortex_memory', 'Person')  = 0;
SELECT create_vlabel('cortex_memory', 'Episode') WHERE _label_id('cortex_memory', 'Episode') = 0;

SELECT create_elabel('cortex_memory', 'ABOUT')        WHERE _label_id('cortex_memory', 'ABOUT')        = 0;
SELECT create_elabel('cortex_memory', 'DERIVED_FROM') WHERE _label_id('cortex_memory', 'DERIVED_FROM') = 0;
SELECT create_elabel('cortex_memory', 'SUPERSEDES')   WHERE _label_id('cortex_memory', 'SUPERSEDES')   = 0;
SELECT create_elabel('cortex_memory', 'MENTIONS')     WHERE _label_id('cortex_memory', 'MENTIONS')     = 0;
SELECT create_elabel('cortex_memory', 'EXPERT_IN')    WHERE _label_id('cortex_memory', 'EXPERT_IN')    = 0;

-- AGE has no native uniqueness constraint, and MERGE without an index degrades
-- to a label scan. Every vertex property map carries a tenant-scoped `key`, so
-- index it per label.
CREATE INDEX IF NOT EXISTS cortex_memory_memory_key_idx
    ON cortex_memory."Memory" (ag_catalog.agtype_access_operator(properties, '"key"'::agtype));
CREATE INDEX IF NOT EXISTS cortex_memory_topic_key_idx
    ON cortex_memory."Topic" (ag_catalog.agtype_access_operator(properties, '"key"'::agtype));
CREATE INDEX IF NOT EXISTS cortex_memory_person_key_idx
    ON cortex_memory."Person" (ag_catalog.agtype_access_operator(properties, '"key"'::agtype));
CREATE INDEX IF NOT EXISTS cortex_memory_episode_key_idx
    ON cortex_memory."Episode" (ag_catalog.agtype_access_operator(properties, '"key"'::agtype));

COMMIT;
