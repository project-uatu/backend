# Uatu backend

Uatu is an open source contributor observer. It follows people, not projects,
and builds a history of their public open source activity across development
platforms.

This repository contains the Go backend. It collects activity from public
sources, stores it, and exposes it through an HTTP API. The frontend will live
in a separate repository.

Uatu is in early development. The current version is a small scaffold: it
collects public GitHub repository data for configured users, stores it in
PostgreSQL, and serves it through a read-only API. Broader contribution
history and the versioned API are in progress.

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
curl http://localhost:3000/github/users/octocat/repositories
```

Set `GITHUB_TOKEN` to raise GitHub API rate limits. The token is optional for
local development. `sslmode=disable` is for this local-only database; use
verified TLS for shared or production databases.

| Variable | Default | Purpose |
|---|---|---|
| `UATU_DATABASE_URL` | Required | PostgreSQL connection URL |
| `UATU_DB_SSLMODE` | `verify-full` | TLS mode if the URL omits `sslmode` |
| `UATU_DB_MIN_CONNS` | `0` | Minimum connections in the pool |
| `UATU_DB_MAX_CONNS` | `10` | Maximum connections in the pool |
| `UATU_DB_CONNECT_TIMEOUT` | `5s` | Timeout when opening a database connection |
| `UATU_ADDR` | `:3000` | HTTP listen address |
| `UATU_USERS` | | Comma-separated GitHub users for `collect` |
| `GITHUB_TOKEN` | | Optional GitHub API token |

Run `go run ./cmd/uatu help` to see the available commands.

## Running with Docker

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
