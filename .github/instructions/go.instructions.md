---
description: 'Go conventions for Uatu, based on Effective Go, Go Code Review Comments, Go Test Comments, the official module layout guide and the Google Go Style Guide'
applyTo: '**/*.go,**/go.mod,**/go.sum'
---

# Go conventions

Write simple, clear, idiomatic Go. When rules conflict, prefer clarity, then
consistency with the surrounding code, then brevity.

Sources: [Effective Go](https://go.dev/doc/effective_go),
[Code Review Comments](https://go.dev/wiki/CodeReviewComments),
[Test Comments](https://go.dev/wiki/TestComments),
[Module layout](https://go.dev/doc/modules/layout),
[Google Go Style Guide](https://google.github.io/styleguide/go/).

## Formatting and tooling

- All code must be `gofmt`-clean. Group imports as: standard library, a blank line, then third-party and module packages.
- Before finishing a change, run `gofmt -l .`, `go vet ./...` and `go test ./...`. Run `go mod tidy` after any dependency change.
- Do not add a dependency when the standard library does the job.

## Project layout

- Uatu is a server, so it has no public Go API. Put the binary's entry point in `cmd/uatu/` and all other packages under `internal/`. Do not add packages under `pkg/`.
- Organize packages by what they provide (for example, `store`, `devstats` or `github`), not by kind (for example, `models`, `handlers` or `utils`).
- Each file has exactly one `package` clause. A new file uses the same package name as the other files in its directory.
- Keep `main` thin: parse configuration, connect dependencies, run, and translate errors into exit codes.

## Naming

- Package names are short, lowercase and singular, with no underscores. Avoid `util`, `common`, `helper`, `types` and `api`.
- Do not repeat the package name in exported names. Prefer `store.Open` over `store.OpenStore`, and `devstats.Client` over `devstats.DevStatsClient`.
- Use MixedCaps and keep initialisms in a consistent case: `userID`, `ServeHTTP`, `apiURL`, never `userId` or `ApiUrl`.
- Do not add a `Get` prefix to accessors or fetchers. Use noun-like names for functions that return values and verb-like names for functions that perform actions.
- Use short receiver names, such as `s` for `*Store`, and keep them consistent across methods. Never use `this` or `self`.
- Do not use a local variable name that shadows an imported package, such as `url` or `errors`.

## Documentation

- Every package has a package comment beginning `// Package name ...`. The main package's comment begins `// Command uatu ...`.
- Every exported identifier has a doc comment that is a full sentence beginning with the identifier's name.
- Comment only to explain why something is done or to clarify non-obvious behavior. Do not restate the code.

## Errors

- Never discard an error with `_` without a comment explaining why it is safe.
- Check each error immediately, handle it first, then return. Keep the success path at the left margin and avoid `else` after `return`.
- Add context with `fmt.Errorf("verb object: %w", err)`. Error strings start in lowercase and do not end in punctuation.
- Handle an error once: either log it or return it, but not both.
- Use sentinel errors such as `var ErrNotFound = errors.New("...")` or custom error types when callers need to tell errors apart. Check them with `errors.Is` and `errors.As`.
- Use `(value, ok bool)` or an error to signal a missing result. Do not return special in-band values such as `""` or `-1`.
- Do not `panic` for expected failures. Panic only on programmer errors that should be impossible.

## Context

- Any function that performs I/O or may block takes `ctx context.Context` as its first parameter.
- Never store a context in a struct. Call `context.Background()` only in `main`, in tests, or in a shutdown path that must outlive a cancelled context.
- Pass the request context through each layer, including handler, service, store and outbound HTTP calls.

## API design

- Accept interfaces and return concrete types.
- Define an interface in the package that uses it, only once it has a real use. Do not define interfaces solely for mocking.
- Prefer explicit dependencies, such as constructor arguments and struct fields, over package-level variables. Mutable globals are not allowed.
- Design types so their zero values are useful, or provide a `New...` constructor that returns a valid value.
- Pass small values by value. Use pointer receivers when a method mutates the receiver or the type is large, and be consistent within a type.
- Keep side-effect imports rare. A database driver may be blank-imported in the package that opens it, so the registration remains next to database initialization.
- Avoid renaming imports unless names collide. When they do, rename the more local or project-specific import.

## HTTP

- Servers use `net/http` with Go 1.22+ `ServeMux` patterns such as `GET /devscores/{user}`. Set explicit timeouts on `http.Server`.
- Outbound clients are structs holding a configured `*http.Client` with a timeout, a base URL and credentials. They do not hold per-request state.
- Build each outbound request with `http.NewRequestWithContext`. Always close response bodies and check status codes.
- Validate path and query input. Return appropriate status codes, and do not expose internal error details to clients.
- Encode JSON with tagged structs. Do not build JSON from strings.

## Concurrency

- Know how every goroutine exits. Tie goroutine lifetimes to a context or an explicit shutdown.
- Use `sync.WaitGroup.Go` because `go.mod` targets Go 1.25 or later. Use `errgroup` when concurrent tasks return errors.
- Protect shared state with a mutex, or pass ownership through channels. Only senders close channels.
- Run `go test -race ./...` for code that uses concurrency.

## Logging and configuration

- Use `log/slog` for structured logs, with keys such as `source`, `user` and `err`. Do not log secrets or tokens.
- Read configuration once at startup into a typed struct, and pass needed values explicitly. Lower-level packages must not call `os.Getenv`.

## Persistence (PostgreSQL target; ADR 0001)

- Use pure-Go `pgx`; keep queries in `internal/store`. PostgreSQL queries use numbered parameter placeholders (`$1`, `$2`); never interpolate external values into SQL.
- Make schema changes as ordered, versioned migrations with database-level serialization. Never edit a migration after it has been released.
- Keep transactions short, fetch upstream outside job-claim transactions, and enforce lease ownership on progress writes. Budget connection pools across replicas.
- Store timestamps as UTC.

## Testing

- Keep tests next to the code in `_test.go` files.
- Name tests `TestFunction_scenario`, and write table-driven tests with `t.Run` subtests.
- Failure messages state the call, the actual value and the expected value, in the form `Func(x) = got, want want`. Use `t.Fatalf` only when the test cannot continue.
- Mark test helpers with `t.Helper()`. Use `t.TempDir`, `t.Cleanup` and `t.Context()` for test setup and cleanup.
- Unit tests must not use the network. Use `net/http/httptest` for HTTP clients and handlers. Until the PostgreSQL store lands, existing store tests use temporary SQLite files; PostgreSQL integration tests use an isolated local/CI database, never an upstream service or production database.
- Cover both success and failure paths. Add or update tests with every behavior change.
- Prefer the standard library. Use `github.com/google/go-cmp/cmp` for comparing complex values.

## Security

- Validate all external input, including path parameters, query strings and upstream responses.
- Bound reads from upstream responses with `io.LimitReader`.
- Use `crypto/rand` for anything security-related. Never commit credentials.
