# Uatu backend

Uatu is an open source contributor observer. It follows people, not projects,
and builds a history of their public open source activity across development
platforms.

This repository contains the Go backend. It collects activity from public
sources, stores it, and exposes it through an HTTP API. The frontend will live
in a separate repository.

Uatu is in early development. The current version is a small scaffold: it
collects public GitHub repository data for configured users, stores it in
SQLite, and serves it through a read-only API. PostgreSQL support, broader
contribution history and the versioned API are in progress.

## Getting started

You need Go 1.26 or later.

```sh
go run ./cmd/uatu collect octocat
go run ./cmd/uatu serve
curl http://localhost:3000/github/users/octocat/repositories
```

Set `GITHUB_TOKEN` to raise GitHub API rate limits. The token is optional for
local development.

| Variable | Default | Purpose |
|---|---|---|
| `UATU_DB_PATH` | `data/uatu.db` | SQLite database path |
| `UATU_ADDR` | `:3000` | HTTP listen address |
| `UATU_USERS` | | Comma-separated GitHub users for `collect` |
| `GITHUB_TOKEN` | | Optional GitHub API token |

Run `go run ./cmd/uatu help` to see the available commands.

## Running with Docker

Each release publishes an image for `linux/amd64` and `linux/arm64` to
`ghcr.io/project-uatu/backend`, tagged with the version (for example `0.1.0`),
the minor version (`0.1`) and `latest`. Use that image in place of `uatu`
below, or build it locally:

```sh
docker build -t uatu .
docker volume create uatu-data
docker run --rm -v uatu-data:/data -e GITHUB_TOKEN -e UATU_USERS=octocat uatu collect
docker run --rm -p 3000:3000 -v uatu-data:/data uatu
```

The image runs a static binary as a non-root user. Mount a volume at `/data`
to keep collected data between runs.

## Development

```sh
gofmt -l .
go vet ./...
go test ./...
go test -race ./...
```

## Learn more

- [Implementation plan](docs/plan.md)
- [Architecture decision records](docs/adr/README.md)
- [CI and release process](docs/release-process.md)
- [GitHub issues](https://github.com/project-uatu/backend/issues)
