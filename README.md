# Uatu backend

Uatu is an open source contributor observer. It follows people, not projects,
and builds a history of their public open source activity across development
platforms.

This repository contains the Go backend. It collects activity from public
sources, stores it, and exposes it through an HTTP API. The frontend will live
in a separate repository.

Uatu collects public GitHub contribution history for tracked people, stores
raw records and normalized timeline actions in PostgreSQL, and serves
time-bounded history, source aggregates and collection coverage through a
private, operator-authenticated API. GitHub release coverage is limited to
known public repositories; the API does not claim complete account-wide
release history.

GitLab.com and Codeberg are collected too. The person API takes an optional
`forge` (`github.com` by default, `gitlab.com` or `codeberg.org`), and
`POST /v1/persons/{id}/sync` accepts `?forge=`. Both forges identify accounts by
their numeric ID, store only public projects and repositories, and need no
token for public data. GitLab covers authored issues and merge requests, known
public projects' releases and recent approval events. Codeberg is read from the
public activity feed (issues, pull requests, merges, reviews, releases) and its
heatmap as aggregate counts; history the feed no longer reaches is reported as a
coverage gap. Merge and review actions are attributed to the person who
performed them: a GitLab merge request merged by someone else is recorded as
opened only. Confidential GitLab issues and private projects are never stored.
A source limit that retrying cannot close, such as the Codeberg feed depth, stays
a visible coverage gap but does not make every later sync repeat the backfill.
A worker only claims jobs for the forges it has a connector for.

## Getting started

Use the released container image,
`ghcr.io/project-uatu/backend:latest`, to try Uatu without installing Go or
building the backend. Follow [Running with Docker](#running-with-docker) below.

`collect octocat` resolves the immutable GitHub account ID, tracks the account,
and runs a resumable public-history backfill from the account's creation date.
The collector records authored issues and pull requests, reviews, releases in
known public repositories, source-reported contribution counts and search
coverage gaps. Repeating the command reuses the person record and schedules an
incremental scan with a seven-day overlap after a successful backfill.

`GITHUB_TOKEN` is required for all `collect` modes: GitHub GraphQL requires
authentication for stable account refresh and contribution aggregates. Serving
stored data does not require a GitHub token, but does require `UATU_API_KEY`.
Repeating `collect` while work is queued or interrupted resumes its original
window and saved cursor.
Searches are restricted to public results. The collector verifies repository
visibility before emitting search events and never persists private repository
activity. `sslmode=disable` is for this
local-only database; use verified TLS for shared or production databases.

`collect` with no user arguments processes due queued event work once, and
`collect --loop` continuously polls for work. `UATU_USERS` can provide usernames
for a one-shot `collect`.

Known public repositories are retained across scans and their visibility is
rechecked before reuse. Review backfills search PRs created before the person's
account as well, since those PRs can receive later reviews.

Contribution counts are daily UTC observations derived from public-repository
GraphQL contributions, not whole-window totals assigned to one date. Private,
internal and restricted contributions are excluded. `daily_total` sums the
reported commits, issues, pull requests and reviews; it is not GitHub's profile
calendar total. Capped aggregate windows are split; unresolved daily caps are
recorded as coverage gaps. Repeated observations are retained, not additive.

After collection, use the logged person ID to query the timeline and source
metadata. `serve` requires `UATU_API_KEY`, a random secret of at least 32 bytes.
Keep it on a trusted server-side frontend/proxy; never embed it in browser code,
a URL or local storage. CORS is disabled unless exact origins are configured
with `UATU_CORS_ORIGINS`. It is not a substitute for authentication. The API
contract is documented in [OpenAPI](docs/openapi/v1.yaml).

`from` and `to` are required RFC3339 timestamps for activity, coverage and
counts; activity uses the half-open range `[from,to)`, with a default page size
of 100 and maximum 500. Requests require a bearer token:

```sh
export UATU_API_KEY="$(openssl rand -hex 32)"
curl -X POST 'http://localhost:3000/v1/persons' \
  -H "Authorization: Bearer $UATU_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"login":"octocat","display_name":"Octocat"}'
curl -H "Authorization: Bearer $UATU_API_KEY" \
  'http://localhost:3000/v1/persons/<person-id>/activity?from=2020-01-01T00:00:00Z&to=2021-01-01T00:00:00Z'
curl -H "Authorization: Bearer $UATU_API_KEY" \
  'http://localhost:3000/v1/persons/<person-id>/coverage?from=2020-01-01T00:00:00Z&to=2021-01-01T00:00:00Z'
curl -H "Authorization: Bearer $UATU_API_KEY" \
  'http://localhost:3000/v1/persons/<person-id>/counts?from=2020-01-01T00:00:00Z&to=2021-01-01T00:00:00Z'
```

| Variable | Default | Purpose |
|---|---|---|
| `UATU_DATABASE_URL` | Required | PostgreSQL connection URL |
| `UATU_DB_SSLMODE` | `verify-full` | TLS mode if the URL omits `sslmode` |
| `UATU_DB_MIN_CONNS` | `0` | Minimum connections in the pool |
| `UATU_DB_MAX_CONNS` | `10` | Maximum connections in the pool |
| `UATU_DB_CONNECT_TIMEOUT` | `5s` | Timeout when opening a database connection |
| `UATU_ADDR` | `:3000` | HTTP listen address |
| `UATU_HEALTH_ADDR` | `:3001` | `collect --loop` health listener address |
| `UATU_API_KEY` | Required for `serve` | Operator bearer key; at least 32 bytes |
| `UATU_CORS_ORIGINS` | Empty | Comma-separated exact HTTP(S) origins allowed by browser clients |
| `UATU_USERS` | | Comma-separated GitHub users to track and collect |
| `GITHUB_TOKEN` | Required for `collect` when GitHub is enabled | GitHub API token for identity refresh and public contribution counts |
| `GITLAB_TOKEN` | Optional | Read-only GitLab.com token for `collect`; raises rate limits, never stored or logged |
| `CODEBERG_TOKEN` | Optional | Read-only Codeberg token for `collect`; raises rate limits, never stored or logged |
| `UATU_FORGES` | All | Comma-separated enabled forges: `github.com`, `gitlab.com`, `codeberg.org` |

Migration `007` renames the GitHub-named tables to `source_accounts` and
`known_repositories` and keeps updatable views under the old names so the
previous release keeps working during a rolling update. Do not create GitLab or
Codeberg work until every replica runs this release; the previous release has no
connector for those sources. A later release drops the views.

`serve` and `collect` do not change the database schema. Run `uatu migrate`
once before starting or upgrading them; they refuse to start against a schema
that lacks a migration they need. Concurrent `migrate` runs are serialized by a
database lock.

Run `docker run --rm ghcr.io/project-uatu/backend:latest help` to see the
available commands.

## Running with Docker

Each release publishes an image for `linux/amd64` and `linux/arm64` to
`ghcr.io/project-uatu/backend`, tagged with the version (for example `0.1.0`),
the minor version (`0.1`) and `latest`. The examples use `latest` to run the
most recent release. You need Docker and a GitHub token; no source
checkout, Go installation or local image build is required.

Anonymous pulls require the container package to be public. If `docker pull`
returns `unauthorized` or `denied`, the repository owner must make the package
public as described in [the release process](docs/release-process.md#container-image),
or you must authenticate to GHCR with an account that has package access.

```sh
export GITHUB_TOKEN='<your-token>'
export UATU_API_KEY="$(openssl rand -hex 32)"
export UATU_IMAGE=ghcr.io/project-uatu/backend:latest
docker pull "$UATU_IMAGE"
docker network create uatu
docker run -d --name uatu-postgres --network uatu \
  -e POSTGRES_USER=uatu -e POSTGRES_PASSWORD=local-only -e POSTGRES_DB=uatu \
  postgres:17
docker run --rm --network uatu -e UATU_DATABASE_URL='postgres://uatu:local-only@uatu-postgres:5432/uatu?sslmode=disable' \
  "$UATU_IMAGE" migrate
docker run --rm --network uatu -e UATU_DATABASE_URL='postgres://uatu:local-only@uatu-postgres:5432/uatu?sslmode=disable' \
  -e GITHUB_TOKEN "$UATU_IMAGE" collect octocat
docker run --rm --network uatu -p 3000:3000 -e UATU_API_KEY \
  -e UATU_DATABASE_URL='postgres://uatu:local-only@uatu-postgres:5432/uatu?sslmode=disable' "$UATU_IMAGE" serve
```

The image contains a static binary and runs as a non-root user. PostgreSQL is
separate from the application container; configure a durable database for
anything beyond local testing.

## Kubernetes runtime contract

The Helm chart lives in a separate repository
([ADR 0008](docs/adr/0008-kubernetes-delivery-contract.md)). This repository
defines the contract it relies on:

- **Commands:** API `serve`, worker `collect --loop`, and a one-shot `migrate`
  run as a pre-install/pre-upgrade Job before the other workloads. Migrations
  must stay compatible with the previous release while a rolling update runs.
- **Probes:** `GET /healthz` (liveness; never checks the database) and
  `GET /readyz` (readiness; checks the database and schema) on `UATU_ADDR` for
  the API and on `UATU_HEALTH_ADDR` for the worker.
- **Configuration:** environment only. Put `UATU_DATABASE_URL`,
  `UATU_API_KEY` (API) and `GITHUB_TOKEN` (worker) in Secrets. Keep replicas x
  `UATU_DB_MAX_CONNS` below the database's `max_connections`.
- **Container:** numeric non-root user 65532, read-only root filesystem, no
  capabilities. Give API pods a short `preStop` sleep (about five seconds) so
  endpoints are removed before the listener stops.
- **Databases:** use the external PostgreSQL when configured and a provisioned
  one otherwise, never falling back from a failing external database. A
  provisioned single instance is not highly available; use a managed database
  for HA. Back up and restore PostgreSQL itself (for example `pg_dump -Fc` and
  `pg_restore`). Workers retry through database outages and leases recover
  interrupted work.

`test/e2e/run.sh` exercises this contract with plain manifests on a throwaway
kind cluster, or on any cluster with two or more nodes
(`CONTEXT=<context> IMAGE=<pullable image> test/e2e/run.sh`). It covers
spread across nodes, hardened security contexts, serialized migrations,
migration-before-start, rolling update and pod termination, worker crash,
database outage, connection limits, backup/restore, both database modes and
no fallback. Lease expiry and stale-token writes are covered by the PostgreSQL
integration tests (`UATU_TEST_DATABASE_URL`).

## Development

Running from source requires Go 1.26 or later and a PostgreSQL database.
For a local development database:

```sh
docker run --rm --name uatu-postgres \
  -e POSTGRES_USER=uatu \
  -e POSTGRES_PASSWORD=local-only \
  -e POSTGRES_DB=uatu \
  -p 5432:5432 postgres:17
```

In another terminal, configure the database URL and run the collector and API
from your source checkout:

```sh
export UATU_DATABASE_URL='postgres://uatu:local-only@localhost:5432/uatu?sslmode=disable'
export GITHUB_TOKEN='<your-token>'
export UATU_API_KEY="$(openssl rand -hex 32)"
go run ./cmd/uatu migrate
go run ./cmd/uatu collect octocat
go run ./cmd/uatu serve
```

For local image development, build with `docker build -t uatu:dev .` and use
`uatu:dev` instead of the released image. Local builds are not needed to test a
release.

Before submitting code changes, run:

```sh
gofmt -l .
go vet ./...
go test ./...
go test -race ./...
```

Store integration tests run against an isolated PostgreSQL database when
`UATU_TEST_DATABASE_URL` is set; otherwise they are skipped. CI starts a
dedicated PostgreSQL service for these tests. Never point the test variable at
a development or production database.

## License and contributing

Uatu is licensed under the [Apache License 2.0](LICENSE), the license used by
CNCF and Linux Foundation projects. Contributions are accepted under the
[Developer Certificate of Origin](https://developercertificate.org/): sign off
every commit with `git commit -s`; see [CONTRIBUTING.md](CONTRIBUTING.md). Commits are also expected to be signed and to
follow lowercase conventional commits. New Go files start with
`// SPDX-License-Identifier: Apache-2.0`.

## Learn more

- [Implementation plan](docs/plan.md)
- [Architecture decision records](docs/adr/README.md)
- [CI and release process](docs/release-process.md)
- [GitHub issues](https://github.com/project-uatu/backend/issues)
