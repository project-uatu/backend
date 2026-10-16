# 0004: Person, identity and event data model

Status: Proposed

Date: 2026-10-16

Issue: [#3](https://github.com/project-uatu/backend/issues/3)

Decision authority: Maintainer acceptance required before implementation.

## Context

ADR 0001 decides the database, driver, migration approach and deployment
contract. It does not decide what the database stores. This ADR covers only
the data model: how people, identities and events relate.

The design document gives each raw event a single `person_id` and treats a
watermark as the newest event seen. Both break down in practice: one event can
involve several tracked people, identity evidence can conflict, and the newest
event seen does not prove that earlier history was fully scanned. The model is
hard to change once data exists, so it is decided before implementation.

## Alternatives

| Option | Pros | Cons |
|---|---|---|
| One event row per person | Simple person timeline queries | Duplicates source data, makes shared events and deduplication difficult |
| One canonical event with person-role associations | Deduplicates source records and represents multiple actors and roles | Requires joins for person timelines |
| Put identities directly on `persons` | Simple initial model | Cannot preserve conflicting evidence or an audit trail |
| Separate identity keys, claims and current assignments | Preserves raw keys, competing evidence and auditable resolution | More tables and explicit identity-resolution work |

## Decision

Keep identity normalization and claim decisions in `internal/identity`; the
schema itself lives in `internal/store` per ADR 0001. Store timestamps as
`timestamptz` in UTC and source payloads as `jsonb`.

### Identity model

- `persons` has a UUID primary key and nullable display name.
- `forge_instances` identifies a provider and canonical base URL. GitHub.com
  is an instance; self-hosted Forgejo instances are separate instances.
- `identity_keys` stores the original value, its kind, optional forge instance,
  and an application-computed normalized value. Account IDs and logins are
  instance-scoped; email keys are global. Partial unique indexes distinguish
  global keys from instance-scoped keys without treating SQL `NULL` values as
  equal.
- Preserve the original identifier. Normalize logins according to their
  provider's comparison rules. For email matching, trim surrounding whitespace
  and lowercase the domain only; preserve the local part. Do not fuzzy-match
  names or infer aliases from Gravatar hashes.
- `identity_claims` is append-only evidence: key, proposed person, method,
  evidence, disposition and timestamps. Multiple claims may conflict.
  `identity_assignments` records the current accepted mapping and allows at
  most one assigned person per key. Changing an assignment requires a new
  claim; retain prior claims for audit.
- Treat immutable forge account IDs as the stable account key. A rename adds a
  login key to the same account; it does not replace the account ID or create
  a new person.

### Event and provenance model

- `raw_events` stores source instance, source external ID, kind, occurrence
  time, raw JSON payload and payload schema version. Enforce uniqueness on
  `(forge_instance_id, external_id)`.
- `normalized_events` stores query-facing fields derived from a raw event,
  keyed by raw event, normalizer version and action. One raw record can yield
  several contribution actions, each with its own time: a pull request can be
  opened, merged and closed. Re-normalization creates a new version rather
  than overwriting prior output; exactly one version per raw event is current,
  and queries read only current output.
- `event_people` links a normalized event to zero or more persons and roles
  such as author, reviewer or commenter. A person is never a mutable owner
  column on a raw event. Associations are derived from current identity
  assignments, and are re-derived when an assignment changes.
- `sync_runs` records person, source instance, start/finish, status and a
  structured fetch report. `sync_coverage` records completed time ranges and
  gaps separately from `checkpoints`, which hold opaque source cursors and
  backfill progress. An observed newest event is not itself a completed
  coverage boundary.
- `jobs` stores durable work and the fields required by ADR 0002: due and
  retry times, status, attempts, lease expiry and a fencing token. Job-claim
  and checkpoint-update semantics are implemented under issue #4.
- Add indexes for identity-key lookup, person/event timeline order, source
  external-ID deduplication, due jobs, and coverage lookups. Add foreign keys
  for all ownership relations. Use `ON DELETE CASCADE` for person-scoped
  assignments, claims, event associations, sync state and jobs. Keep raw
  source events independent of persons; payload erasure and retention policy
  must be settled in the privacy work under issue #10 before public profile
  endpoints are delivered.

### Person history over time

The primary query is a person's contributions over time, bucketed by day,
week, month or year and broken down by kind, source and project.

- `event_people` carries a copy of the event's occurrence time and is indexed
  on `(person_id, occurred_at)`, so timeline and bucketed queries do not scan
  every event.
- An action's occurrence time comes from the source and never changes when the
  raw payload is refreshed. History is append-only: collecting again adds or
  corrects actions but does not move existing ones in time.
- `contribution_counts` stores source-provided aggregate counts, such as
  GitHub's daily contribution calendar, per person, forge instance, day and
  kind, with the time they were observed. These cover activity that cannot be
  enumerated as individual events. They are kept separate from events and are
  never converted into fabricated individual events.
- Charts distinguish "no activity" from "not collected" by reading
  `sync_coverage` for the requested range.
- Buckets are computed on the fly from the index. Precomputed summaries are
  added only when measurement shows they are needed.

## Consequences

Person timelines require joins through normalized events and event roles.
Identity resolution can retain conflicting evidence without prematurely
merging people. Raw records can be normalized repeatedly, and one source event
can be attributed to multiple tracked people.

The schema supports privacy-oriented removal of person-scoped associations,
but raw payload retention and complete takedown semantics require a separate
privacy decision before public profile APIs.

## Validation

- Review and accept this ADR before implementing the schema or replacing the
  SQLite store.
- Verify foreign keys, source-scoped deduplication, conflicting identity
  claims, one current assignment per key, multiple event-person roles,
  normalizer-version coexistence and person-scoped cascade behavior.
- Verify a multi-year person timeline bucketed by month and kind uses the
  `(person_id, occurred_at)` index, counts each action once across normalizer
  versions, and reports uncovered ranges as gaps rather than zero activity.
- Verify re-collecting a person does not move existing actions in time, and
  re-deriving associations after an assignment change updates the timeline.
- Database, migration and configuration checks follow ADR 0001's validation.
