# memory-service

The temporal memory layer — what separates Cortex from ordinary RAG. It knows
what is *currently* true, not merely what was written.

The service consumes `ContentEvent`s from NATS JetStream, extracts candidate
memories with two LLM passes, screens them through eight deterministic checks,
and commits the survivors to a bi-temporal store in Postgres (with an Apache AGE
graph projection). It serves the read path to the query gateway and the
decisions timeline.

```
ContentEvent (NATS JetStream)
        │
        ▼
   ┌─────────────────────────────────────────────┐
   │ broad pass (~10K-char chunks)               │  what would someone need to know?
   │ detail pass (names / prices / versions)     │  the specifics, verbatim
   └─────────────────────────────────────────────┘
        │  merge on normalized topic
        ▼
   verifier — 8 checks, rejects on doubt
        │
        ▼
   classify: fact | event | instruction | task | decision
        │
        ▼
   normalized-topic keying → conflict resolution → edge invalidation
        │
        ▼
   ┌─────────────────────────────────────────────┐
   │ bi-temporal store (valid_from/to, ingested) │
   │ provenance index (memory ↔ episode)         │
   │ people / expertise edges                    │
   │ Postgres + Apache AGE                       │
   └─────────────────────────────────────────────┘
        │                              ▲
        ▼                              │
   Query Gateway · Decisions Timeline   decay + consolidation jobs
```

---

## The ideas that carry the design

**Two clocks, not one.** `valid_from`/`valid_to` are *world* time: when the
statement was true. `ingested_at` is *system* time: when Cortex learned it. A
Slack message from Tuesday ingested on Friday is valid from Tuesday but ingested
Friday, and both are needed — the first answers "what was true on Wednesday",
the second answers "what did we know on Wednesday", which is the question audits
actually ask.

**Supersession, never deletion.** When a newer memory arrives on the same
normalized topic, the older one's `valid_to` is set to the newer one's
`valid_from` and `superseded_by` is linked forward. The row stays. A decisions
timeline is only possible if what was replaced is still readable, and a memory
that was cited in an answer last quarter must still resolve.

**Topic normalization is the hinge.** Two memories conflict only if they key to
the same topic, so a key that is too loose invalidates unrelated facts and one
that is too tight lets a stale Confluence page survive a Slack decision that
plainly overrides it. The rules — casefold, strip punctuation, drop stopwords,
fold trivial plurals, sort tokens — make "pricing for the enterprise plan" and
"enterprise plan pricing" the same key, while leaving `v0.6.3`, `1099-MISC` and
`eu-central-1` untouched.

**Two extraction passes, because one prompt cannot do both jobs.** The broad
pass reads a large window and is good at gist and terrible at specifics; it will
write "pricing was raised" and drop the number. The detail pass asks only for
particulars, with the source span each came from. Extraction chunks are ~10K
characters — much larger than retrieval chunks — because the decision at the
bottom of a thread only makes sense with the debate above it.

**The verifier is the cheapest quality lever in the system.** A bad memory does
not merely fail to help: it is durable, citable, and it *evicts* a good memory
from the same topic. So the screen is deterministic and rejects on doubt.

**Permission parity applies to memory too.** Memories inherit their episode's
ACLs, search filters on group overlap, and a fetch by ID rechecks authorization
in the handler — returning 404 rather than 403, because "this exists but is not
for you" leaks that it exists.

---

## The eight checks

Run in order, cheapest first. Every rejection is persisted with its reason: the
rejection *mix* tells you whether to fix the prompt, the chunker, or the model.

| # | Check | Rejects |
| --- | --- | --- |
| 1 | `shape` | Empty, too short, too long, unknown kind, low confidence, questions |
| 2 | `topic` | Missing or generic topics that would collapse unrelated memories into one chain |
| 3 | `grounding` | Quotes not present in the source — the wholesale-fabrication catch |
| 4 | `self_contained` | "They decided to go with the second option" — unreadable six months later |
| 5 | `kind_agreement` | A bare assertion labelled a decision, a task with no owner or action |
| 6 | `temporal` | Missing or future `valid_from`, intervals that run backwards |
| 7 | `attribute_fidelity` | Invented prices, versions and figures — the most damaging output possible |
| 8 | `redundancy` | Restatements of a candidate already accepted from the same episode |

Checks 3 and 7 can pass with a *penalty* instead of rejecting: a missing quote is
a weakness, not a lie, so the memory survives with reduced confidence.

---

## Conflict resolution

For each verified candidate, the store locks the current memory on
`(tenant, kind, topic)` and takes one of four paths:

| Outcome | When | Effect |
| --- | --- | --- |
| `created` | No current memory on the topic | Insert |
| `reinforced` | Same claim, no attribute disagreement | Extend provenance, bump salience, no new row |
| `superseded` | Newer, and it disagrees | Close the old one at the new `valid_from`, link forward, insert |
| `backfilled` | *Older* than what is current | File into history, closed at the current memory's start; current is untouched |

Events never take the supersession path — two things that happened do not
contradict each other.

`isRestatement` decides reinforcement versus supersession and is the most
consequential predicate in the package. Too loose and a genuine reversal is
swallowed, leaving the stale memory current. It requires both substantially
identical text *and* no disagreement on any shared attribute — which is what
catches "pricing is $40/seat" against "pricing is $50/seat", near-identical
sentences that mean opposite things.

The invariant is enforced in the database, not just in Go: a partial unique
index permits exactly one open-ended memory per `(tenant, kind, topic)`.

---

## The worked example

A Confluence page from January states the deploy policy. A Slack thread in March
reverses it. Both normalize to the topic `deploy-freeze-policy`.

On ingesting the Slack thread, the store closes the Confluence-derived memory at
the decision timestamp and links `superseded_by`. Afterwards:

- a query for what is true now returns the Slack decision, cited to the message
  permalink;
- a query `as_of` February returns the Confluence policy;
- `GET /v1/memories/history?topic=deploy+freeze+policy&kind=decision` returns
  both, in order, with the moment the change took effect.

This is covered end to end by `TestSlackDecisionSupersedesStaleConfluencePage`.

---

## API

All requests carry `X-Cortex-Tenant-Id` and `X-Cortex-Acl-Groups` (comma
separated, already expanded by the query gateway against SpiceDB — this service
applies groups, it does not re-expand them; a second expansion implementation
would be a second place for permission bugs to live).

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/v1/memories/search` | Memory injection for the query gateway; ACL-filtered, optional `as_of` |
| `GET` | `/v1/memories/{id}` | One memory with provenance and what it replaced |
| `GET` | `/v1/memories/history?topic=&kind=` | Full supersession chain for a topic |
| `GET` | `/v1/decisions/timeline?topic=&from=&to=` | Decisions newest-first, each with what it replaced |
| `GET` | `/v1/experts?topic=` | Who knows about X |
| `GET` | `/v1/stats` | Counts and the rejection mix, for the admin console |
| `POST` | `/v1/episodes` | Direct ingest — eval harness, single-document replay, connectors not on JetStream |
| `GET` | `/healthz`, `/readyz` | Liveness and readiness |

---

## Maintenance jobs

**Decay** applies exponential decay on a per-kind half-life from the last
reinforcement, and *archives* rather than deletes — an archived memory drops out
of default search but still resolves for an `as_of` query and still backs the
answers that cited it. Decisions and instructions are exempt from archival
entirely: a decision nobody has mentioned in two years is usually one so settled
that nobody argues about it, which is exactly the institutional memory this
layer exists to hold.

**Consolidation** folds topics that have accumulated past usefulness into one
summary memory, inheriting the union of provenance so citability survives. It
refuses to merge memories that share no ACL group, since the summary would carry
content across a permission boundary.

---

## Configuration

| Variable | Default | Notes |
| --- | --- | --- |
| `MEMORY_POSTGRES_DSN` | — | Required |
| `MEMORY_ADDR` | `:8087` | |
| `MEMORY_GRAPH_ENABLED` | `false` | Apache AGE projection; every v1 query works without it |
| `MEMORY_AUTO_MIGRATE` | `false` | Evaluation mode only — production applies the same embedded SQL from a Helm job |
| `MEMORY_NATS_URL` | `nats://localhost:4222` | |
| `MEMORY_MAX_INFLIGHT` | `8` | Bounded: the backfill spike becomes GPU spend if it is not |
| `MEMORY_ACK_WAIT` | `5m` | Must exceed worst-case extraction; the per-message deadline is this minus 30s |
| `MEMORY_MAX_DELIVER` | `5` | Backoff schedule is trimmed to fit |
| `MEMORY_PURGE_ON_DELETE` | `false` | Right-to-be-forgotten hard delete instead of invalidation |
| `MEMORY_CONSUMER_DISABLED` | `false` | Accept episodes only over HTTP |
| `MEMORY_LLM_BASE_URL` | `http://llm-gateway:8080` | Any OpenAI-compatible endpoint |
| `MEMORY_JOBS_ENABLED` | `true` | |
| `MEMORY_DECAY_INTERVAL` | `6h` | |
| `MEMORY_CONSOLIDATE_INTERVAL` | `24h` | |
| `MEMORY_SALIENCE_FLOOR` | `0.15` | Archive threshold |

---

## Development

```sh
make test-unit          # everything needing a server skips itself
make test-integration   # starts Postgres+AGE and NATS in Docker, runs everything
make lint
docker compose up -d    # single-node evaluation mode
```

Integration tests are gated on `MEMORY_TEST_DSN`, `MEMORY_TEST_GRAPH` and
`MEMORY_TEST_NATS`. They are integration tests on purpose: the invariants worth
testing live in the SQL schema and in JetStream's delivery semantics, so a fake
store would test nothing that can actually break.

---

## Layout

```
cmd/memory-service/     entrypoint and wiring
internal/contentevent/  the JetStream wire contract and its mapping to episodes
internal/bus/           durable pull consumer, ack/nak/term policy
internal/extract/       chunking, broad pass, detail pass, merge
internal/verify/        the eight checks
internal/memory/        domain model, topic normalization, similarity
internal/store/         bi-temporal store, conflict resolution, AGE projection
internal/pipeline/      extract → verify → commit
internal/jobs/          decay and consolidation
internal/api/           read path for the query gateway and timeline UI
migrations/             embedded schema
```
