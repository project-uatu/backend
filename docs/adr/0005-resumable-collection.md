# 0005: Resumable bounded collection and normalization

Status: Accepted

Date: 2026-10-17

Issue: [#4](https://github.com/project-uatu/backend/issues/4)

Decision authority: Accepted by the maintainer on 2026-10-17.

Prerequisites: accepted ADRs [0001](0001-postgresql-backend-scope.md),
[0002](0002-kubernetes-collector.md) and
[0004](0004-person-identity-event-model.md).

## Context

ADR 0001 selects PostgreSQL and stateless replicas; ADR 0002 selects a
continuous collector that claims durable work; ADR 0004 provides raw events,
normalized actions, source-scoped checkpoints, coverage, sync reports and
leased jobs. This ADR decides how to process that state safely across retries,
partial failures, crashes and multiple collector replicas.

The original connector sketch returns an unbounded event slice and combines
emails and logins as strings. A full backfill can exhaust memory, and a failed
late page can lose all earlier progress. Advancing a checkpoint to the newest
observed event would also claim coverage of history that may never have been
scanned.

## Alternatives

| Option | Advantages | Costs |
|---|---|---|
| Return the complete history from each connector | Small interface | Unbounded memory; no durable page progress |
| Let connectors write directly to the store | Few intermediate values | Couples clients to persistence and transaction policy; hard to test |
| Return bounded pages to the collector | Bounded memory; collector controls persistence and checkpoints | Requires explicit cursor and partial-page semantics |
| Normalize before storing raw events | One write path | A normalizer failure can discard source evidence; reprocessing needs a refetch |
| Persist raw pages, then normalize through durable work | Preserves evidence and permits replay without the source | Adds a small PostgreSQL work queue and a second processing stage |

## Decision

### Package and connector boundary

- Keep orchestration and the connector interface in `internal/collector`.
  Define the interface there, where it is consumed.
- Keep shared raw-event, page, actor-role and normalized-action values in
  `internal/event`; typed `Source`, `Kind` and key kinds belong to those domain
  values. The event package may use `internal/identity` keys but imports neither
  connectors, collector, normalizer nor store.
- Keep each upstream client in its own internal package (for example,
  `internal/github`). A connector converts provider responses to the event
  contract; it does not import `internal/store` or write to PostgreSQL.
- Keep identity-key normalization in `internal/identity` and event
  transformation in `internal/normalizer`. The normalizer is a pure transform
  from a raw event to zero or more normalized actions and actor-role keys.
- Use `json.RawMessage` for source payloads. Actor keys carry both kind and
  value; forge-scoped keys also carry their forge instance. Preserve original
  values and use ADR 0004's identity normalization rules.
- Keep dependencies one-way: source clients and the normalizer depend on
  `internal/event`; the collector orchestrates clients, normalizer, identity
  resolution and store; `internal/store` persists event values but imports no
  connector or collector package.

### Bounded fetch and reports

- A connector fetches one bounded page per call using a request containing the
  tracked identity, a half-open time window, and an opaque cursor. A zero
  window start means history from the beginning. The returned page contains
  validated raw events, its next cursor and a completion flag. Return a report
  by value alongside the page and error; it remains valid when the fetch returns
  an error and must be recorded in the `sync_run`. Reports contain request/page
  counts, rate-limit state, known gaps and sanitized failure details; they
  never contain credentials.
- A failed or malformed response may include already validated events. Persist
  those as new raw records and enqueue normalization work for them with
  conflict-do-nothing semantics, but do not replace existing payloads or
  advance the cursor. On a complete successful page, upsert the validated raw
  records and enqueue their normalization work. All writes are idempotent by
  `(forge_instance_id, external_id)`.
- Fetch upstream outside database transactions. Do not hold a job, quota or
  event lock while making a request.

### Persistence, normalization and progress

- Persist a successful page's raw events, durable normalization work items and
  next opaque cursor in one short transaction. A failed page may persist
  previously unseen valid raw records, their normalization work items and its
  report in one short transaction, but it does not move the cursor. Thus a
  retry may fetch the page again without losing acknowledged progress or
  replacing good data with a failed response. Any database error rolls back
  that page transaction.
- Normalize only committed raw events. Process bounded normalization work in
  separate short transactions; write the current normalized action version and
  event-person links atomically. A work item is completed only after that
  transaction commits. Retries and re-normalization are idempotent using the
  unique raw-event/version/action keys from ADR 0004. Re-normalization to a new
  version queues existing raw records without fetching upstream again.
- Keep backfill progress distinct from incremental scan progress. Backfill
  advances through the source cursor/history; incremental scans cover a
  bounded interval from the last completed boundary minus a configurable
  overlap through a fixed run end. Updating an old event does not move its
  occurrence time. Only a fully completed interval with no unrecorded gaps is
  marked covered; gaps and partial runs remain visible in `sync_runs` and
  `sync_coverage`.
- A watermark records completed coverage, never the greatest event timestamp
  observed. Store page cursor progress separately from completed interval
  coverage. A failed or cancelled scan cannot mark unscanned history complete.

### Jobs, leases and quotas

- Claim due collection and normalization work in short PostgreSQL transactions
  using `FOR UPDATE SKIP LOCKED`. Persist due time, attempt count, bounded
  exponential retry time with jitter, upstream rate-limit reset, lease expiry
  and a unique claim token. Use the token on every progress, retry and completion
  write so an expired worker cannot update work reclaimed by another worker.
- Coalesce duplicate pending sync requests for the same person, forge instance
  and source; enforce the existing idempotency key where callers supply one.
  Collection and normalization writes remain safe under at-least-once delivery.
- Coordinate rate-limit reservations in PostgreSQL by provider/credential
  bucket. Store bucket state and reset times, never access tokens. Respect
  `Retry-After` and provider reset times; stop retrying permanent failures and
  record them visibly.
- One-shot `collect` processes due work and exits. The `collect --loop` worker
  polls and drains the same processing primitive as specified by ADR 0002.
  Cancellation stops new claims and cancels in-flight requests; unfinished
  claims are conditionally released when safe or expire for recovery.

## Consequences

Memory use is bounded by a source page and a normalization batch. Raw evidence
survives normalizer failures and provider outages, and retries do not depend on
re-downloading already committed data. PostgreSQL is the only queue and
coordination service.

Connectors must expose stable opaque cursors and report limits or gaps rather
than imply complete history. A source whose API cannot establish a completed
range must leave that range uncovered. Durable normalization work adds database
writes and a second worker stage. At-least-once delivery still permits repeated
upstream requests; idempotency prevents those repeats from duplicating stored
events or actions.

Source-specific endpoint suitability, search caps and enumeration strategies
remain in issue #5 and require a separate source-strategy ADR.

## Validation

- Test bounded page sizes, typed actor keys, valid reports on partial failure,
  sanitized reports, and no cursor advancement or payload replacement after a
  failed page.
- Test that a successful page atomically persists raw events, normalization
  work and its cursor; a partial page persists new valid events, work and report
  without moving the cursor; and a database error rolls back its transaction.
- Test raw persistence before normalization, replay after normalizer failure,
  idempotent repeated normalization and re-normalization with a new version.
- Against isolated PostgreSQL, test concurrent duplicate enqueue/claims,
  lease expiry, stale-token write rejection, retry and rate-limit due times,
  cancellation, crash/restart, partial pages and uncovered scan intervals.
- Verify one-shot processing exits and loop mode stops cleanly on cancellation.
- Run `gofmt -l .`, `go vet ./...`, `go test ./...` and `go test -race ./...`.
  Unit tests use fixtures and do not depend on upstream access or a real
  `GITHUB_TOKEN`.
