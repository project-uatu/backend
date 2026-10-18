# Uatu repository instructions

Uatu is a lightweight, containerized Go service. It collects public open source
activity for developers and uses PostgreSQL through pure-Go `pgx` (ADR 0001).
The binary has two modes: `collect` fetches and stores data, then exits, and
`serve` is an HTTP API that reads from stored data.

## Required for every change

- Commits use lowercase conventional commits without issue numbers, are signed
  off by the human author (`-s`) and signed (`-S`). Agents must not add
  `Signed-off-by` or `Co-authored-by`; disclose assistance with an
  `Assisted-by: <agent>/<model>` trailer. See `CONTRIBUTING.md`.

- Follow `.github/instructions/go.instructions.md` for all Go code.
- Keep the executable in `cmd/uatu/` and application packages in `internal/`. This server has no public Go API; do not add project packages under `pkg/`.
- Keep Uatu lightweight. Use a single static binary (`CGO_ENABLED=0`), a distroless non-root image, and PostgreSQL through pure-Go `pgx`. Do not add Redis or external message queues.
- Canonical persistent state belongs in PostgreSQL. Support multiple stateless API/collector replicas with transactional job claims, leases and idempotency. Failed fetches must never overwrite stored data.
- Future Helm deployment uses an external PostgreSQL database when configured, otherwise provisions PostgreSQL. Database HA is a separate concern from application replicas.
- Keep upstream access in explicit clients, collection/persistence orchestration in `collector`, HTTP handling in `server`, and environment parsing in `config`.
- Update `README.md` when behavior, configuration, endpoints or commands change.
- Keep this repository backend-only; the frontend lives in a separate repository.
- Target Kubernetes first. Standalone-container deployment support comes later.
- Before implementing a major change, create an ADR under `docs/adr/` and obtain acceptance. Link the ADR from the implementation issue; a proposed ADR is not approval to implement.

## Validation

Run these commands before finishing:

```sh
gofmt -l .
go vet ./...
go test ./...
go test -race ./...
```

Unit tests must not depend on the network or on a real `GITHUB_TOKEN`.
PostgreSQL integration tests use an isolated local/CI database and must not
contact upstream services.
