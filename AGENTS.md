# AGENTS.md

Guidance for AI coding agents working in this repository. Read it before making
changes, and follow it together with the human contributor's instructions.

The full repository rules are in
[`.github/copilot-instructions.md`](.github/copilot-instructions.md) and the Go
conventions in [`.github/instructions/go.instructions.md`](.github/instructions/go.instructions.md).
[`CONTRIBUTING.md`](CONTRIBUTING.md) describes the contribution process.

## Essentials

- Uatu is a lightweight Go service with two modes: `collect` and `serve`. State
  lives in PostgreSQL through pure-Go `pgx`. Do not add Redis or message queues.
- Keep the executable in `cmd/uatu/` and packages in `internal/`. Do not add
  packages under `pkg/`. This repository is backend-only.
- Major changes need an accepted ADR under `docs/adr/` before implementation.
  Keep ADR changes on a docs branch with no code.
- Update `README.md` when behavior, configuration, endpoints or commands change.
  Do not edit `CHANGELOG.md`, which release-please generates.
- Start new Go files with `// SPDX-License-Identifier: Apache-2.0`.
- Never commit secrets. Unit tests must not use the network or a real token.

## Validation

Run before finishing, and make sure CI will pass:

```sh
gofmt -l .
go vet ./...
golangci-lint run
go test ./...
go test -race ./...
```

Store tests need `UATU_TEST_DATABASE_URL` pointing to an isolated database.

## Commits and pull requests

- Use strict lowercase conventional commits, with no issue numbers in commit
  subjects or PR titles.
- The human author signs off (`git commit -s`) and signs (`-S`) commits. Do not
  add `Signed-off-by` or `Co-authored-by`. Disclose assistance with an
  `Assisted-by: <agent>/<model>` trailer.
- Disclose AI assistance in the pull request description. Keep descriptions,
  messages and comments concise.
- Resolve review conversations once you have addressed them.
