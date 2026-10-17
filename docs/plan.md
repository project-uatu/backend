# Uatu backend: status and implementation plan

Updated: 2026-10-17

## Confirmed scope

- Canonical module: `github.com/project-uatu/backend`.
- PostgreSQL through pure-Go `pgx`; support multiple API/collector replicas.
- Future Helm deployment uses an external PostgreSQL database when configured;
  otherwise it provisions PostgreSQL. The maintainer will create the Helm code.
- No external queue; use durable PostgreSQL job state.
- Backend only; the frontend will be built in a separate repository.
- Kubernetes first; standalone-container packaging later.
- Create and accept an ADR before every major implementation change.

The [ADR index](adr/README.md) distinguishes accepted decisions from proposals.
This plan supersedes the earlier session-only plan. It does not implement the
future commands, schema or endpoints described below.

## Current status

The repository is a small Go scaffold: `serve`, one-shot `collect`, typed
startup configuration, a PostgreSQL connection pool and initial person/event
schema, structured logging, stdlib HTTP routing, tests and a static non-root
container.

GitHub currently lists owned repository names, not contributions. The
PostgreSQL schema now represents persons, identities, raw and normalized
events, coverage and collector state. The existing repository-list endpoint
still uses a temporary snapshot table until issue #5 replaces it with
contribution history. The HTTP API is read-only and never fetches from
upstream (ADR 0003).
There is no Kubernetes deployment or versioned person-centric API yet.

The module path and imports have now been corrected and legacy-organization
references audited. CI pins golangci-lint v2.14.0, which supports Go 1.26.

## Collector scheduling in Kubernetes

| Approach | Pros | Cons |
|---|---|---|
| CronJob + one-shot `collect` | No idle worker resources; native schedule, retries and Job history; easy batch resource/deadline settings; can access PostgreSQL from any eligible node | New-pod startup; manual sync waits for the next run unless separately triggered; long jobs can skip schedules |
| Always-running collector | Prompt queue pickup; natural jitter/backoff; no repeated pod startup; workers can scale independently of the API | Idle memory; application scheduling/recovery code; worker health monitoring; shared quota coordination across workers |

**Accepted in ADR 0002:** separate API and continuous collector
workloads sharing PostgreSQL, with bounded worker concurrency. Retain one-shot
`collect` as the job-processing primitive. This remains batch collection,
not real-time streaming.

PostgreSQL removes the SQLite same-node/shared-volume requirement. API replicas
can scale independently; collector scaling requires transactional claims,
expiring leases, stale-worker ownership checks, idempotency and coordinated
upstream quotas. Do not hold database locks during upstream requests.

A CronJob remains reasonable for hourly/daily freshness. `Forbid` reduces overlap
but is not an exactly-once guarantee. Both approaches need durable job state.
[ADR 0001](adr/0001-postgresql-backend-scope.md) accepts PostgreSQL and the
external-or-provisioned Helm database contract. [Accepted ADR 0002](adr/0002-kubernetes-collector.md)
covers scheduling. Database HA and
backup/restore remain necessary even with multiple application replicas.

## Corrections to the original design

- Use `internal/collector`, `internal/server` and explicit internal source
  packages. No public connector tree or embedded frontend.
- Replace SQLite with PostgreSQL using ordered, serialized migrations. Nothing
  has shipped, so no SQLite data transition is needed. Do not maintain two
  database backends. Design rolling-version schema compatibility. Keep the
  temporary repository snapshot table until issue #5 delivers GitHub history.
- Stream bounded pages/batches instead of returning full histories in memory.
  Define interfaces at consumers, typed actor keys and event kinds, and
  `json.RawMessage` payloads. Design package dependencies to avoid import cycles.
- Other return values are not inherently invalid when a Go function returns an
  error. Explicitly document that a fetch report remains valid on partial failure.
- Keep raw-event identity independent of a tracked person. An event can involve
  several people/roles; use associations rather than one mutable `person_id`.
  Namespace external IDs and account keys by forge instance as well as source.
- Prefer immutable forge account IDs for rename tracking. A 404 is not proof of
  a rename, and public email search cannot reliably recover an account.
- Emails and avatar hashes are evidence, not universal proof of ownership.
  Preserve original values, define normalization carefully (email local parts
  are not universally case-insensitive), support conflicts/time bounds, and do
  not auto-merge using Gravatar or guessed noreply aliases.
- A watermark is a completed scan boundary, not simply the newest observed event.
  Preserve raw pages on failure but never mark unscanned history as covered.
  Separate backfill and incremental progress and account for updated old events.
- GitHub activity and contribution aggregates do not guarantee full history.
  Verify pagination/search limits and endpoint support, split bounded searches,
  and publish inaccessible or capped ranges as gaps. Reviews need a dedicated
  enumeration strategy; do not assume a generic author search enumerates them.
- Shallow clones cannot establish complete commit history. Evaluate upstream
  commit APIs before cloning; any clone cache is disposable, bounded and outside
  canonical persistent state. Approve its filesystem policy in an ADR first.
- Document auth, raw/identity access, privacy and CORS for a separate frontend.
  Aggregate counts must not be emitted as fabricated individual events.

## Ordered implementation issues

Order is a delivery recommendation; explicit dependencies identify real gates.
For every implementation: reference an accepted ADR, update directly related
documentation and run the repository's required Go checks. Unit tests remain
offline; PostgreSQL integration tests use isolated local/CI databases, never
upstream services or a real GitHub token. No frontend work belongs in these issues.

| Step | GitHub issue |
|---|---|
| 1. Scheduling and storage ADR | [#1](https://github.com/project-uatu/backend/issues/1) |
| 2. Scaffold and CI | [#2](https://github.com/project-uatu/backend/issues/2) |
| 3. Identity/event schema | [#3](https://github.com/project-uatu/backend/issues/3) |
| 4. Resumable collection | [#4](https://github.com/project-uatu/backend/issues/4) |
| 5. GitHub history | [#5](https://github.com/project-uatu/backend/issues/5) |
| 6. Versioned API | [#6](https://github.com/project-uatu/backend/issues/6) |
| 7. Kubernetes delivery | [#7](https://github.com/project-uatu/backend/issues/7) |
| 8. GitLab and Codeberg | [#8](https://github.com/project-uatu/backend/issues/8) |
| 9. Commit history | [#9](https://github.com/project-uatu/backend/issues/9) |
| 10. Identity/privacy lifecycle | [#10](https://github.com/project-uatu/backend/issues/10) |
| 11. Gerrit and mailing lists | [#11](https://github.com/project-uatu/backend/issues/11) |
| 12. Standalone and later extensions | [#12](https://github.com/project-uatu/backend/issues/12) |

### 1. Decide PostgreSQL collector scheduling and replica coordination

Dependencies: none.

Use accepted ADR 0001 as the storage baseline. Done: ADR 0002 was accepted on
2026-10-16 with continuous collector workers.

Acceptance:
- Confirm or revise ADR 0002's MVP assumptions: daily scheduled sync, manual
  work normally starts within one minute, one initial collector replica, and
  scale-out after measuring queue age and throughput.
- Decide continuous workers versus CronJob, including the manual-trigger path.
- Accept the PostgreSQL job-claim, lease/fencing, retry, quota-coordination,
  shutdown, migration and rollout behavior specified by the ADR.
- Do not implement a daemon or manifests until the decision is accepted.

### 2. Align the scaffold with backend scope and reliable CI

Dependencies: none.

[ADR 0003](adr/0003-read-only-scaffold-api.md) (accepted) removes misleading
routes and makes GET read-only.

Done: CI lint pinned to golangci-lint v2.14.0 with `golangci-lint-action@v9`
(commit `56bb1eb`).

Acceptance:
- Remove `GET /gh/users/{user}/contributions` (it returns owned repository
  names, not contributions) and the placeholder root response.
- GET serves stored data or a JSON `404`; it never calls upstream or writes.
  Remove `collector.UpstreamError` and the server's collector dependency, and
  shorten the write timeout that was sized for upstream fetches.
- Add the unversioned, read-only `GET /github/users/{user}/repositories`,
  returning JSON encoded from a tagged struct. Validate `{user}` as a GitHub
  login and return `400` for invalid input.
- Validate `collect` arguments before opening the database. Print usage for
  unknown commands and exit with status 2. Make the collection loop testable
  offline; cover unknown command, missing users, partial failure and
  cancellation.
- Move `actions/checkout` and `actions/setup-go` to Node 24 releases; keep the
  static image build.
- Update the README.

Out of scope: case-insensitive login keys, raw payloads and the PostgreSQL
schema (#3, #5), and the versioned API (#6).

### 3. Migrate to PostgreSQL and implement the person, identity and event schema

Dependencies: accepted ADRs 0001, 0002 and 0004.

ADR 0001 accepts PostgreSQL. Keep PostgreSQL queries in `internal/store` and
identity rules in `internal/identity`.

[Accepted ADR 0004](adr/0004-person-identity-event-model.md) defines the
person, identity and event data model. Database, migration and configuration
choices follow ADR 0001. Detailed collection, checkpoint and job-processing
behavior remains in issue #4 under accepted ADR 0002. Person-associated
removal is handled in the schema; payload erasure and takedown policy are
deferred to issue #10.

Acceptance:
- Add persons, instance-scoped identities with stable account IDs, auditable
  claims and conflict handling.
- Add raw events, event/person role associations, normalized events, sync runs,
  coverage/checkpoints and durable job state.
- Define deduplication, deletion, schema/normalizer versions and query indexes.
- Replace the SQLite driver/configuration with pure-Go `pgx`, explicit
  connection/TLS/pool settings and ordered, serialized migrations.
- Remove the SQLite driver and file-based migrations; nothing has shipped, so
  no data transfer is required and no dual backend is kept. Retain only the
  temporary PostgreSQL snapshot table used by the current repository route,
  pending replacement in #5.
- Use foreign keys and UTC values; test concurrent clients, migration
  serialization and rolling-version compatibility against isolated PostgreSQL.

### 4. Build resumable bounded collection and normalization

Dependencies: steps 1 and 3.

[Accepted ADR 0005](adr/0005-resumable-collection.md) covers connector
contracts, import boundaries, transaction boundaries, normalization and
checkpoint semantics.

Acceptance:
- Stream bounded pages with typed actor keys, kinds and raw JSON; valid reports
  are returned and persisted on partial failure.
- Commit raw data before replayable normalization; retries do not duplicate
  events or overwrite good data with failed fetches.
- Separate completed scan intervals, backfill progress and incremental overlap.
- Persist due times, rate-limit resets, retry times and expiring job claims.
- Use transactional PostgreSQL claims and ownership-checked progress writes;
  fetch upstream outside transactions and coordinate shared-credential quotas.
- Retain one-shot collection; add loop mode as specified by accepted ADR 0002.
- Test cancellation, duplicate jobs, stale claims, crash/restart and partial
  pages without relying on network access.

### 5. Deliver GitHub history with honest coverage

Dependencies: step 4.

[Accepted ADR 0006](adr/0006-github-event-enumeration.md) defines bounded
REST Search for authored issues, pull requests and review discovery, with
separate paginated review and known-repository release enumeration. Its
account-wide release-coverage limitation was accepted by the maintainer.
The decision uses GraphQL contribution data only for separate aggregate counts.

Replace owned-repository names with person-centric
contribution records.

Acceptance:
- Resolve immutable account IDs and collect authored PRs/issues, reviews and
  releases using supported, paginated enumeration paths.
- Use bounded date partitions/checkpoints for capped searches; record coverage
  gaps and rate limits rather than promise complete public history.
- Keep aggregate contribution counts distinct from individual timeline events.
- Add rate-limit-aware cancellation/retry behavior and sanitized fixture tests.
- Demonstrate initial backfill, incremental overlap and repeat-sync idempotency.

### 6. Publish the versioned backend API for the separate frontend

Dependencies: steps 3 and 4; GitHub data from step 5 supports end-to-end delivery.

Accept an API/auth/privacy ADR before adding the public contract.

Acceptance:
- Implement `/v1/persons` onboarding/profile/identifier changes and authenticated,
  idempotent sync enqueueing with a job/status reference.
- Provide activity keyset pagination, summaries, projects and coverage queries.
- Restrict identity resolution/raw payload access; validate requests, bound
  bodies and define API-key/read-policy handling.
- Document timestamp, cursor, error, response-envelope and CORS contracts in
  OpenAPI, including the intentional deployment-specific origins.
- Keep GET upstream-free; no HTML/templates/charts or frontend repository work.

### 7. Ship and exercise the Kubernetes deployment

Dependencies: steps 1, 2, 5 and 6.

Use accepted ADR 0001 and accepted ADR 0002. The maintainer will create
Helm deployment code; backend work supplies and exercises its runtime contract.

Acceptance:
- Exercise multiple API/worker replicas across nodes with non-root security
  contexts, resource limits, Secrets and startup/readiness/liveness.
- Define Helm integration for external PostgreSQL when configured and
  chart-provisioned PostgreSQL otherwise; no fallback on external DB failure.
- Serialize migrations before dependent workloads start and exercise rolling
  upgrades, termination, lease expiry, stale-worker writes and restart recovery.
- Exercise both database modes, outage handling, connection limits and
  PostgreSQL backup/restore. Do not imply bundled PostgreSQL provides HA.

### 8. Add GitLab.com and Codeberg connectors

Dependencies: steps 4 and 6.

Accept source-specific ADRs for identity lookup, coverage and API usage.

Acceptance:
- Reuse the bounded connector/report contract without source-specific core schema
  migrations; distinguish forge instances in identifiers and deduplication.
- Collect supported public contributions incrementally and state history gaps.
- Respect per-host quotas, public/private boundaries and descriptive User-Agent.
- Use sanitized fixtures for pagination, failures and restart/idempotency.

### 9. Add bounded seeded-repository commit history

Dependencies: steps 5 and 8.

Accept an ADR comparing upstream commit APIs against clone-based extraction,
including branch coverage, attribution and resource/cache policy.

Acceptance:
- Bound repository discovery and request/CPU/disk budgets per person and host.
- Prefer supported upstream enumeration; use pure-Go cloning only where justified.
- Report shallow/truncated branches and unavailable history as incomplete.
- Match verified evidence without assuming every commit email is an account claim.
- Keep disposable clone caches non-authoritative; test pruning, restarts,
  malformed repositories and static distroless compatibility.

### 10. Deliver identity administration and privacy lifecycle

Dependencies: steps 3 and 6.

Accept an ADR for audited merges/splits, evidence changes and deletion semantics.

Acceptance:
- Provide authenticated merge/split, rename and claim/conflict operations.
- Prefer immutable forge IDs; never infer renames from 404s alone.
- Support identity changes without destroying raw evidence or producing stale
  normalized associations; make re-normalization auditable.
- Define takedown, export/redaction, retention and deletion across derived data
  and backups; keep raw identities out of default public responses.
- Document ToS/public-data restrictions before public profile redistribution.

### 11. Add Gerrit and mailing-list history

Dependencies: steps 4 and 10.

Accept source ADRs covering instance discovery, queries and identity confidence.

Acceptance:
- Add bounded configurable Gerrit instances and public archive collection.
- Define archive access from verified documentation, not an assumed REST API.
- Require suitable identity evidence/time bounds and handle reused email addresses.
- Preserve public provenance, coverage gaps and rate limits with offline tests.

### 12. Support standalone containers and later product extensions

Dependencies: step 7 for standalone packaging; steps 6 and 10 for product scope.

Deferred until the Kubernetes MVP is stable. Accept separate ADRs before each
extension; this is a follow-on planning issue, not a single large implementation.

Acceptance:
- Document one-shot and loop-mode standalone deployment with
  PostgreSQL, secrets, graceful shutdown and the same replica safety guarantees.
- Split approved JSON/CSV exports, OIDC/OAuth self-onboarding, StackExchange,
  public sharing and tenant isolation into bounded follow-up issues.
- Materialize summaries only after measurements justify it; do not introduce an
  external queue without a separate accepted ADR.
