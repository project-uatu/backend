# Uatu, the open source watcher

Uatu collects public open source activity for developers and stores the
results in a SQLite database, so they persist between runs.

This is the backend module, `github.com/project-uatu/backend`. The frontend
will live in a separate repository. Kubernetes is the primary deployment
target; standalone-container packaging is planned later.

The executable is in `cmd/uatu`. Application code is under `internal/`, divided
by responsibility: `collector` coordinates fetches and persistence, `github`
is the upstream client, `store` manages SQLite and schema
migrations, `server` exposes the HTTP API, and `config` loads process settings.
The HTTP layer uses Go's standard library. The resulting executable remains a
static binary suitable for the distroless container.

## Commands

| Command | Description |
|---|---|
| `uatu serve` (default) | Starts the HTTP API on port 3000. Responses come from the database; data is fetched and stored only the first time a user is requested. |
| `uatu collect [user ...]` | Fetches fresh data for the given users (or `UATU_USERS`), stores it, and exits. Run it on a schedule to keep data current. |

Endpoints:

- `GET /gh/users/:user/contributions` - repositories owned by the user
- `GET /healthz` - returns `204 No Content` when the HTTP server is running

Stored responses include a `Last-Modified` header with the collection time.
If an upstream request fails, the API returns a generic JSON `502` response;
the internal error is written to structured logs. Collection failures are
logged and cause the `collect` command to exit nonzero after attempting the
other requested sources and users.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `UATU_DB_PATH` | `data/uatu.db` (`/data/uatu.db` in the container) | SQLite database file |
| `UATU_ADDR` | `:3000` | Listen address for `serve` |
| `UATU_USERS` | | Comma-separated GitHub users for `collect` |
| `GITHUB_TOKEN` | | Optional token for higher GitHub rate limits |

## Running in a container

The image is a static binary on a distroless base, running as a non-root user.
Mount a volume at `/data` to keep results between runs:

```sh
docker build -t uatu .
docker volume create uatu-data

# Collect once and exit (e.g. from cron, a systemd timer or a Kubernetes CronJob)
docker run --rm -v uatu-data:/data -e GITHUB_TOKEN -e UATU_USERS=octocat uatu collect

# Serve stored results
docker run -d -p 3000:3000 -v uatu-data:/data uatu
```

SQLite supports one writer at a time, so avoid overlapping `collect` runs and
do not share the volume between multiple replicas.

## Development

```sh
gofmt -w cmd internal
go test ./...
go vet ./...
go test -race ./...
go run ./cmd/uatu collect octocat
go run ./cmd/uatu
```

The first database open runs transactional SQLite migrations and preserves
existing snapshots. Back up the database volume before deploying a version
that changes the schema.

## Architecture and roadmap

PostgreSQL is the target database, so multiple application replicas can run
([ADR 0001](docs/adr/0001-postgresql-backend-scope.md)). The code and runtime
instructions above still describe the current SQLite scaffold; PostgreSQL
support has not shipped. The maintainer's future Helm deployment will use an
external database when configured, otherwise provision PostgreSQL. Application
replicas do not imply a highly available database.

See the [status and implementation plan](docs/plan.md) for the roadmap and
the Kubernetes collector scheduling comparison. The plan describes future
behavior; the commands and endpoints above describe the current implementation.

Major changes require an accepted [architecture decision record](docs/adr/README.md)
before implementation. Collector scheduling (ADR 0002) is accepted but not yet
implemented.
