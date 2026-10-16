# 0002: Kubernetes collector scheduling

Status: Accepted

Date: 2026-10-16

Issue: [#1](https://github.com/project-uatu/backend/issues/1)

Decision authority: Accepted by the maintainer on 2026-10-16.

## Context

Uatu needs periodic and manually requested collection while the API serves
historical data. [ADR 0001](0001-postgresql-backend-scope.md) makes PostgreSQL
the shared store, so API and collector pods can run on any node. No shared
volume or co-location is required. Collection is batch work, not real-time
streaming.

The MVP assumes daily scheduled sync is sufficient and an accepted manual sync
should normally start within one minute. Initial deployment is expected to
serve a modest number of tracked people; exact population, upstream request
budgets and per-source sync duration will be measured during the MVP. These
assumptions favor a continuously available queue consumer without preselecting
an unmeasured production replica count.

## Alternatives

| Option | Advantages | Costs |
|---|---|---|
| CronJob running one-shot `collect` | No idle workers; Kubernetes handles schedules, retries and Job history; batch resource limits and deadlines are natural | A requested sync waits for the next run or needs a separate Job trigger; pod startup on every run; long backfills can delay later runs |
| Continuous collector Deployment (`collect --loop`) | Starts queued manual work promptly; per-person jitter and backoff without pod churn; workers scale independently of the API | Uses memory while idle; the application owns queue polling, cancellation and recovery; worker health must be monitored |
| Scheduler inside the API process | One workload | Collection failures and resource use affect API serving; no separate resource limits or scaling |

## Decision

Run a continuously running collector Deployment separate from the API:

- **API:** `serve`, scaled independently for read traffic.
- **Collector:** a future `collect --loop`, initially one replica with bounded
  per-replica concurrency. Permit multiple replicas; scale them independently
  when queue-age and throughput measurements justify it.

Keep one-shot `collect` as the job-processing primitive. The loop calls the
same due-job logic repeatedly. This primitive also serves diagnostics and
standalone use later.

The database is the durable queue; do not add a broker. Manual requests create
or coalesce a durable job and return its identifier. Workers poll for due jobs
at a short interval (initial target: five seconds), so ordinary manual work
starts within one minute when a worker is healthy. Scheduled work is represented
by durable per-person/source due times, with jitter to spread upstream traffic.
The exact poll interval and concurrency are configuration/operational
parameters, not guarantees of completion time.

Workers claim jobs in short PostgreSQL transactions using
`FOR UPDATE SKIP LOCKED`, set an expiring lease with a unique claim token, then
release the transaction before fetching. Heartbeats extend leases during long
operations. Every checkpoint and completion write checks the claim token;
expired or superseded workers cannot commit progress. Fetch and persistence are
at-least-once: event upserts and checkpoints must be idempotent. A crash may
cause a page to be fetched again, but must not lose acknowledged progress.

Transient errors retry with bounded exponential backoff and jitter, respecting
upstream `Retry-After`/rate-reset signals. Permanent errors become visible
failed jobs and sync reports rather than retrying forever. Cancellation stops
new claims, cancels in-flight requests, commits only already-safe checkpoints,
and lets unfinished leases expire or release them conditionally.

Workers sharing an upstream credential coordinate rate-limit reservations and
reset times through PostgreSQL. Secrets remain outside the database; database
rows identify a credential/quota bucket without storing its token. The API only
enqueues work. GET requests never fetch upstream.

Run schema migrations as a serialized deployment step before API/worker
rollout, not independently on every replica.

A CronJob remains a valid alternative when delayed sync is acceptable. It needs
`concurrencyPolicy: Forbid`, UTC schedules, bounded retries/deadlines, and a
separate mechanism for promptly starting manually requested jobs. `Forbid` does
not guarantee exactly-once execution, so the same database leases and
idempotency still apply.

## Consequences

No external queue is introduced. Collector replicas can scale independently of
the API. Upstream quotas, database capacity and provider latency may limit
throughput before CPU does; bound both per-replica concurrency and the aggregate
database connection pool. The five-second polling target incurs periodic
database reads while idle. Collector and API resources are budgeted separately.
Long backfills must be resumable, and due times, retry state, quota resets and
checkpoints survive restarts.

At-least-once execution means connectors and persistence must be idempotent.
Leases and heartbeat logic add implementation and operational complexity.
Scaling collectors does not provide exactly-once upstream requests or make a
provisioned PostgreSQL database highly available.

This ADR implements no new CLI mode and no Kubernetes resources; issues #4 and
#7 deliver them.

## Validation

### Accepted assumptions

Acceptance did not require performance or scaling tests; the implementation
could not exist before this ADR was accepted. The maintainer accepted these
assumptions:

- Daily scheduled sync is sufficient.
- A manually requested sync normally starts within one minute.
- One collector replica is deployed initially.

Replica count, per-replica concurrency and poll interval are operational
settings. Tuning them after measurement does not require a new ADR. Replacing
the continuous collector with a CronJob does.

### Implementation checks

The implementation must pass these checks before it is relied on:

- During the MVP, measure tracked-person count, daily event volume, upstream
  quotas, queue age and full-backfill duration before setting production
  replica and concurrency targets.
- With two or more workers against one PostgreSQL database, verify exclusive
  live claims, recovery after lease expiry, rejection of stale-token writes,
  idempotency after process termination, and no lost acknowledged checkpoints.
- Verify that manual enqueue starts within the one-minute target under normal
  load, and that due schedules, jitter, retry backoff and rate-limit resets
  survive restarts.
- Exercise shared-credential quota coordination, bounded connection use,
  graceful termination, migration-before-rollout and rolling updates while API
  replicas remain available.
- Confirm permanent failures are visible and transient failures do not retry
  without bounds.

## References

- [Kubernetes CronJob semantics](https://kubernetes.io/docs/concepts/workloads/controllers/cron-jobs/)
- [Kubernetes Deployments](https://kubernetes.io/docs/concepts/workloads/controllers/deployment/)
- [PostgreSQL `SELECT ... FOR UPDATE SKIP LOCKED`](https://www.postgresql.org/docs/current/sql-select.html#SQL-FOR-UPDATE-SHARE)
