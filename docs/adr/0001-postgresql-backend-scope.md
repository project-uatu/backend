# 0001: PostgreSQL and backend-only scope

Status: Accepted

Date: 2026-10-16

Issue: [#3](https://github.com/project-uatu/backend/issues/3)

Decision authority: Explicit maintainer direction in the planning discussion.

## Context

Uatu must support multiple API and collector replicas on different Kubernetes
nodes. The current scaffold stores snapshots in SQLite, which needs every
database user on one host with a shared filesystem. That rules out independent
replicas.

The maintainer has confirmed:

- PostgreSQL as the database;
- a separate frontend repository;
- Kubernetes-first deployment, with standalone containers later;
- maintainer-owned Helm code that uses an external PostgreSQL database when
  configured and provisions PostgreSQL otherwise.

## Alternatives

- **SQLite with a single co-located pod:** the smallest deployment, but it
  cannot run independent replicas across nodes.
- **Supporting both SQLite and PostgreSQL:** keeps a database-free mode, but
  doubles migration, query and concurrency testing.
- **PostgreSQL only:** adds a database to operate, but supports independent API
  and worker replicas through transactions and durable job state.
- **An embedded frontend:** serves a UI without a separate application, but
  mixes frontend work into the backend repository.

## Decision

- Use PostgreSQL as the only database, through the pure-Go `pgx` driver. Keep
  all SQL in `internal/store`.
- Keep `CGO_ENABLED=0` and the distroless non-root image. Application replicas
  are stateless apart from disposable local caches.
- Use module path `github.com/project-uatu/backend`. Keep the executable under
  `cmd/uatu/` and application code under `internal/`.
- Provide an HTTP API only. Server-rendered UI and client assets belong in the
  frontend repository.
- Allow independent API and collector workloads across nodes. Claim jobs in
  short transactions, for example with `FOR UPDATE SKIP LOCKED`. Fetch from
  upstream outside the transaction.
- Use expiring leases and ownership checks on progress writes so that a stale
  worker cannot commit after another worker has taken over. Delivery is at
  least once, not exactly once.
- Coordinate upstream quotas across workers that share credentials.
- Run ordered, versioned migrations with database-level serialization before
  dependent workloads start. Keep schema changes compatible with rolling
  deployments.
- The Helm deployment uses an external PostgreSQL database when one is
  configured, and otherwise provisions PostgreSQL. Both modes use the same
  application database contract. If a configured external database is
  unreachable, startup fails visibly; it never falls back to a provisioned one.
- Database credentials come from Secrets or environment variables and are never
  logged. The implementation issue specifies connection, TLS and pool settings.
- Design and validate Kubernetes deployment first. Defer standalone packaging.
- Accept an ADR before each major change.

ADR 0002 covers collector scheduling.

## Consequences

Replicas share durable state without a shared application filesystem.
Connection pools must be budgeted across all replicas, because scaling the
application does not scale the database.

PostgreSQL becomes required infrastructure, including for standalone
containers later. A Helm-provisioned PostgreSQL still needs persistent storage,
backups, restores and upgrades, and it is not automatically highly available.
Database high availability is a separate concern from application replicas.

The SQLite driver, configuration, migrations and tests are replaced by issue
#3. No SQLite release has shipped, so no data migration path is needed. A
temporary PostgreSQL snapshot table preserves the current repository-list
scaffold until issue #5 replaces it with contribution history.

The frontend integrates through a documented, versioned HTTP contract. Identity
emails and raw upstream payloads are not public by default.

## Validation

- Module imports use the canonical path, and the binary builds with CGO
  disabled.
- The repository's formatting, vet, normal and race test commands pass.
- Integration tests run against an isolated PostgreSQL instance, with no
  upstream network calls and no real GitHub token.
- Tests cover concurrent replicas, duplicate claims, lease expiry, writes from
  stale workers, crash recovery, migration serialization and rolling-version
  compatibility.
- The Helm work tests both the external and the provisioned database modes:
  credentials, TLS, visible connection failures, backup and restore.
