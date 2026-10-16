# 0003: Read-only scaffold API

Status: Accepted

Date: 2026-10-16

Issue: [#2](https://github.com/project-uatu/backend/issues/2)

Decision authority: Accepted by the maintainer on 2026-10-16.

## Context

The scaffold's HTTP API does not match the design or the accepted ADRs:

- `GET /gh/users/{user}/contributions` returns the names of repositories the
  user owns, stored as a comma-joined string. The name claims contributions it
  does not provide.
- A GET for a user with no stored data fetches from upstream and writes to the
  database. This makes reads depend on upstream availability and quota, lets
  anonymous clients trigger collection, and contradicts ADR 0002, which
  assigns collection to the collector.
- `GET /` returns a placeholder `Hello, World!` response.

No version has been tagged or released, and the frontend does not exist yet,
so these routes have no known consumers. The versioned person-centric API
arrives with issue #6.

## Alternatives

| Option | Pros | Cons |
|---|---|---|
| Keep the routes and deprecate them | No change for hypothetical consumers | Keeps misleading names; deprecation work for routes nobody uses |
| Remove every data route until `/v1` | Smallest API surface | `serve` exposes nothing stored; collected data can only be inspected in the database |
| Remove the routes and keep one honestly named, read-only route | Data stays observable; names match content; GET never collects | A short-lived route that `/v1` replaces |

## Decision

Remove the misleading routes and keep one honestly named, read-only route:

- Remove `GET /gh/users/{user}/contributions` and the placeholder `GET /`.
- Add `GET /github/users/{user}/repositories`. It returns the stored
  repository names as JSON (`{"repositories": [...]}`) with a `Last-Modified`
  header, a JSON `404` when nothing is stored, and a JSON `400` when `{user}`
  is not a valid GitHub login.
- GET handlers only read stored data. Only `collect` (and later the collector
  workers from ADR 0002) fetches from upstream and writes.
- Unversioned routes are a development scaffold, not a supported contract.
  They may change or be removed without a deprecation period. Compatibility
  guarantees start with `/v1` (#6); after that, breaking changes need a new
  API version and a documented deprecation period.

## Consequences

- Requests for users not yet collected return `404` instead of fetching.
  Operators run `uatu collect` first.
- Removed routes return `404`. Clients of the old routes, if any, break
  without notice; this is acceptable because nothing has been released.
- GitHub data is stored under a new source key in JSON form. Existing local
  SQLite databases keep orphaned rows that nothing reads; no migration removes
  them, because #3 replaces the SQLite store.
- The HTTP write timeout no longer needs to cover upstream fetches.
- `/github/users/{user}/repositories` is removed when `/v1` lands.

## Validation

- Handler tests show GET never calls upstream or writes, returns `404` for
  missing data and `400` for invalid logins, and serves stored JSON with
  `Last-Modified`.
- The removed routes return `404`.
- `gofmt -l .`, `go vet ./...`, `go test ./...` and `go test -race ./...` pass
  without network access.
