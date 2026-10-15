# 0002: Kubernetes collector scheduling

Status: Proposed

Date: 2026-10-16

Issue: [#1](https://github.com/project-uatu/backend/issues/1)

Decision authority: Maintainer acceptance required before implementation.

## Context

Uatu needs periodic and manually requested collection while the API serves
historical data. [ADR 0001](0001-postgresql-backend-scope.md) makes PostgreSQL
the shared store, so API and collector pods can run on any node. No shared
volume or co-location is required. Collection is batch work, not real-time
streaming.

## Alternatives

| Option | Advantages | Costs |
|---|---|---|
| CronJob running one-shot `collect` | No idle workers; Kubernetes handles schedules, retries and Job history; batch resource limits and deadlines are natural | A requested sync waits for the next run; pod startup on every run; long backfills can delay later runs |
| Continuous collector Deployment (`collect --loop`) | Picks up queued work quickly; per-person jitter and backoff without pod churn; workers scale independently of the API | Uses memory while idle; the application owns scheduling, cancellation and recovery; worker health must be monitored |
| Scheduler inside the API process | One workload | Collection failures and resource use affect API serving; no separate resource limits or scaling |

## Decision

Recommend separate Deployments:

- **API:** `serve`, with replicas scaled for read traffic.
- **Collector:** a future `collect --loop` with bounded concurrency per replica.

Keep one-shot `collect` as the job-processing primitive. The loop calls the
same due-job logic repeatedly. This primitive also serves diagnostics and
standalone use later.

Both scheduling modes need the same safeguards:

- Persist due times, retry times, rate-limit resets, leases and checkpoints in
  PostgreSQL.
- Claim jobs in short transactions and never hold locks during upstream
  requests.
- Fence progress writes by lease ownership.
- Share quota state across workers that use the same credentials.
- The API only enqueues work. GET requests never fetch from upstream.

A CronJob remains a valid alternative when delayed sync is acceptable. It needs
`concurrencyPolicy: Forbid`, UTC schedules and bounded retries and deadlines.
`Forbid` does not guarantee exactly-once execution, so the same database leases
and idempotency still apply.

## Consequences

No external queue is introduced. Collector replicas can scale, but upstream
quotas, not CPU, are usually the limit. Collector and API resources are
budgeted separately. Long backfills must be resumable, and rate-limit reset
times must survive restarts. Graceful shutdown releases or expires leases.

This proposal implements no new CLI mode and no Kubernetes resources.

## Validation

- Before acceptance, confirm the expected number of tracked persons, sync
  latency and backfill duration.
- Run multiple collector replicas against one PostgreSQL instance. Show that
  there is no duplicate work, that expired leases are recovered, and that a
  stale worker's writes are rejected.
- Specify graceful termination, migration ordering and rolling update behavior.
- Show that shared-credential quotas are respected across replicas.
- If CronJob is chosen instead, show behavior when a run overlaps or fails while
  the API stays available.

## References

- [Kubernetes CronJob semantics](https://kubernetes.io/docs/concepts/workloads/controllers/cron-jobs/)
- [PostgreSQL `SELECT ... FOR UPDATE SKIP LOCKED`](https://www.postgresql.org/docs/current/sql-select.html#SQL-FOR-UPDATE-SHARE)
