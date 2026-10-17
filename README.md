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
read-only API. GitHub release coverage is limited to known public repositories;
the API does not claim complete account-wide release history.

## Getting started

You need Go 1.26 or later and a PostgreSQL database. For a local database,
start PostgreSQL with Docker:

```sh
docker run --rm --name uatu-postgres \
  -e POSTGRES_USER=uatu \
  -e POSTGRES_PASSWORD=local-only \
  -e POSTGRES_DB=uatu \
  -p 5432:5432 postgres:17
```

In another terminal, configure the database URL and run the collector and API:

```sh
export UATU_DATABASE_URL='postgres://uatu:local-only@localhost:5432/uatu?sslmode=disable'
go run ./cmd/uatu collect octocat
go run ./cmd/uatu serve
```

`collect octocat` resolves the immutable GitHub account ID, tracks the account,
and runs a resumable public-history backfill from the account's creation date.
The collector records authored issues and pull requests, reviews, releases in
known public repositories, source-reported contribution counts and search
coverage gaps. Repeating the command reuses the person record and schedules an
incremental scan with a seven-day overlap after a successful backfill.

Set `GITHUB_TOKEN` to increase API limits. It is optional for local development.
The collector verifies repository visibility before storing search results and
never persists private repository activity. `sslmode=disable` is for this
local-only database; use verified TLS for shared or production databases.

`collect` with no user arguments processes due queued event work once, and
`collect --loop` continuously polls for work. `UATU_USERS` can provide usernames
for a one-shot `collect`.

After collection, use the logged person ID to query the timeline and source
metadata. `from` and `to` are required RFC3339 timestamps; `limit` is optional
for activity and defaults to 100 (maximum 500).

```sh
curl 'http://localhost:3000/v1/persons/<person-id>/activity?from=2020-01-01T00:00:00Z&to=2021-01-01T00:00:00Z'
curl 'http://localhost:3000/v1/persons/<person-id>/coverage?from=2020-01-01T00:00:00Z&to=2021-01-01T00:00:00Z'
curl 'http://localhost:3000/v1/persons/<person-id>/counts?from=2020-01-01T00:00:00Z&to=2021-01-01T00:00:00Z'
```

| Variable | Default | Purpose |
|---|---|---|
| `UATU_DATABASE_URL` | Required | PostgreSQL connection URL |
| `UATU_DB_SSLMODE` | `verify-full` | TLS mode if the URL omits `sslmode` |
| `UATU_DB_MIN_CONNS` | `0` | Minimum connections in the pool |
| `UATU_DB_MAX_CONNS` | `10` | Maximum connections in the pool |
| `UATU_DB_CONNECT_TIMEOUT` | `5s` | Timeout when opening a database connection |
| `UATU_ADDR` | `:3000` | HTTP listen address |
| `UATU_USERS` | | Comma-separated GitHub users to track and collect |
| `GITHUB_TOKEN` | | Optional GitHub API token for higher public-data limits |

Run `go run ./cmd/uatu help` to see the available commands.

## Running with Docker

Each release publishes an image for `linux/amd64` and `linux/arm64` to
`ghcr.io/project-uatu/backend`, tagged with the version (for example `0.1.0`),
the minor version (`0.1`) and `latest`. Use that image in place of `uatu`
below, or build it locally:

```sh
docker build -t uatu .
docker network create uatu
docker run -d --name uatu-postgres --network uatu \
  -e POSTGRES_USER=uatu -e POSTGRES_PASSWORD=local-only -e POSTGRES_DB=uatu \
  postgres:17
docker run --rm --network uatu -e UATU_DATABASE_URL='postgres://uatu:local-only@uatu-postgres:5432/uatu?sslmode=disable' \
  -e GITHUB_TOKEN -e UATU_USERS=octocat uatu collect
docker run --rm --network uatu -p 3000:3000 \
  -e UATU_DATABASE_URL='postgres://uatu:local-only@uatu-postgres:5432/uatu?sslmode=disable' uatu
```

The image contains a static binary and runs as a non-root user. PostgreSQL is
separate from the application container; configure a durable database for
anything beyond local testing.

## Development

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

## Learn more

- [Implementation plan](docs/plan.md)
- [Architecture decision records](docs/adr/README.md)
- [CI and release process](docs/release-process.md)
- [GitHub issues](https://github.com/project-uatu/backend/issues)
