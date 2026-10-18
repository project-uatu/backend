# 0008: Kubernetes delivery contract and separate Helm chart repository

Status: Proposed

Date: 2026-10-18

Issue: [#7](https://github.com/project-uatu/backend/issues/7)

Prerequisites: [ADR 0001](0001-postgresql-backend-scope.md),
[ADR 0002](0002-kubernetes-collector.md), #1, #2, #5 and #6.

## Context

Issue #7 requires the backend to be shipped and exercised on Kubernetes. The
maintainer will write the Helm code. The chart will also deploy components
beyond this backend (for example the separate frontend), so it does not belong
only in this backend-only repository.

The current runtime has `serve` and `collect [--loop]`, a `/healthz` endpoint
that does not check the database, and schema migrations tied to opening the
store. It has no dedicated migration command, no readiness endpoint, and no
documented Kubernetes contract. ADR 0002 already requires migrations to run as
a serialized step before API and worker rollout.

## Alternatives

| Option | Advantages | Costs |
|---|---|---|
| Chart in this repository | Chart and code change atomically; one CI | Couples the backend to other components; contradicts backend-only scope; backend releases carry chart changes |
| Chart in a separate repository; backend defines the contract and an e2e harness (proposed) | Chart is versioned independently and can cover several components; backend scope stays clean | Compatibility must be managed across repositories; cross-repository CI |
| Chart in a separate repository; backend does nothing beyond publishing an image | Least backend work | Nothing here verifies the runtime contract; #7 acceptance would be unowned |

## Decision

Keep the Helm chart in a separate maintainer-owned repository. This repository
does not contain chart code. It owns and tests the runtime contract:

- **Commands:** `serve` (API), `collect --loop` (worker) and a dedicated
  one-shot `migrate` command. Replicas of `serve` and `collect --loop` never
  migrate. They verify the schema version is compatible and fail readiness
  otherwise.
- **Migrations:** serialized by a PostgreSQL advisory lock, run by a
  pre-install/pre-upgrade Job (or equivalent) before dependent workloads start.
  Migrations must be compatible with the previous release's replicas during a
  rolling update (expand/contract), or the release notes must say otherwise.
- **Probes:** `/healthz` for liveness (process only, no database check, so a
  database outage does not restart pods); a new `/readyz` that checks database
  connectivity and schema compatibility. A startup probe is allowed for slow
  starts. The worker, which has no HTTP server today, gets a small health
  listener, or an equivalent documented probe.
- **Configuration:** environment variables only. `UATU_DATABASE_URL` and
  `GITHUB_TOKEN` come from Kubernetes Secrets. The contract documents
  `UATU_DATABASE_MAX_CONNS` budgeting: replicas times pool size must stay below
  the server's `max_connections`.
- **Security context:** the image runs as non-root with a read-only root
  filesystem and no extra capabilities. The binary needs no writable paths.
- **Termination:** on SIGTERM, `serve` drains in-flight requests and workers stop
  claiming, cancel fetches and leave unfinished leases to expire (ADR 0002),
  within the default termination grace period.
- **Database modes:** the chart uses an external PostgreSQL when configured and
  provisions one otherwise. There is no fallback from a failing external
  database to a provisioned one. The bundled database is a single instance and
  documentation must not imply high availability. Backup/restore is documented
  for the PostgreSQL itself, not for the application.
- **Exercise harness:** a script and test suite in this repository (under
  `test/e2e/`) runs against a local kind cluster using a pinned chart reference
  from the chart repository, or plain manifests while the chart does not exist.
  It never contacts upstream services; GitHub is stubbed.

The chart repository pins image versions. Backend releases state the
contract version (probes, commands, environment variables) in `README.md` and
the changelog; a breaking contract change needs a new ADR.

## Consequences

- The backend gains a `migrate` command, a readiness endpoint, a worker health
  listener and migration-version checks. Removing migration from replica
  startup is a behavior change for standalone use; `README.md` must say how to
  migrate.
- Cross-repository compatibility is a maintenance cost. The e2e harness
  detects drift.
- Rolling-update safety depends on expand/contract migration discipline.
- The e2e suite needs kind and a container image build, so it is a separate CI
  job and not part of `go test ./...`.
- Provisioned PostgreSQL offers no HA; users needing HA must use an external
  managed database.

## Validation

On kind with multiple nodes, verify:

- Multiple API and worker replicas spread across nodes, running non-root with
  resource limits, Secrets and startup/readiness/liveness probes.
- Migration Job runs once and before workloads, including with concurrent
  installs; replicas refuse to run against an incompatible schema.
- Both database modes. A failing external database causes visible failure and no
  fallback. Database outage turns readiness off without restarting pods, and
  recovery restores service.
- Rolling upgrade keeps the API available; pod termination, lease expiry,
  stale-token writes and restart recovery behave as ADR 0002 requires.
- Total connections stay within the configured limit under maximum replicas.
- A PostgreSQL backup restores to a working deployment with data intact.
- `gofmt -l .`, `go vet ./...`, `go test ./...` and `go test -race ./...` pass;
  unit tests stay offline.

## References

- [Kubernetes probes](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/)
- [Helm chart hooks](https://helm.sh/docs/topics/charts_hooks/)
- [kind](https://kind.sigs.k8s.io/)
