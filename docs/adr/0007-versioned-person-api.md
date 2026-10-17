# 0007: Versioned person API, authentication and privacy

Status: Proposed

Date: 2026-10-17

Issue: [#6](https://github.com/project-uatu/backend/issues/6)

Prerequisites: completed issues [#3](https://github.com/project-uatu/backend/issues/3),
[#4](https://github.com/project-uatu/backend/issues/4) and
[#5](https://github.com/project-uatu/backend/issues/5).

## Context

Uatu has a small read-only `/v1` API for activity, coverage and contribution
counts. A separate frontend needs a stable contract to discover and onboard
people, request collection, and query their history. The current API exposes
stored person history without authentication, has no onboarding or job-status
routes, and returns bare arrays. Adding writes without deciding who can read
person profiles, how sync requests are protected, or what identity data is
returned risks exposing a contributor's aggregated history or enabling
unbounded collection.

The API must remain upstream-independent on reads, safe across stateless
replicas, useful to a separately hosted frontend, and consistent with the
person/identity, durable-job, and provenance models in ADRs 0004 and 0005.
Identity resolution, merges, takedowns, raw-event access and full deletion
semantics are separate concerns; issue #10 addresses their lifecycle.

## Alternatives

| Option | Advantages | Costs |
|---|---|---|
| Public reads and an API key only for writes | Convenient for a public frontend; no read credential distribution | Makes every tracked profile public; consent, takedown and privacy controls are not yet defined |
| One operator bearer key for all person data | Simple to deploy; protects profiles and writes while privacy lifecycle is incomplete | Requires a trusted server-side frontend/proxy; one shared key has no per-user authorization or self-service identity |
| User sessions through OIDC/OAuth | Supports self-onboarding and per-user authorization | Requires user/account lifecycle, provider configuration, session storage and authorization policy beyond this MVP |

## Decision

**Proposed: keep the versioned person API private until the privacy lifecycle
is accepted and implemented.** Require one operator API key for every `/v1`
route, including reads and writes. `GET /healthz` remains unauthenticated and
returns no person or database information. The key is configured as
`UATU_API_KEY`, supplied from a deployment Secret, and sent as
`Authorization: Bearer`. The server fails startup if the key is missing or
shorter than 32 bytes. Compare credentials in constant time; never log them.
Terminate TLS at the ingress or trusted reverse proxy. The key is a deployment
credential, not an end-user identity; all authorized key holders have the same
access.

The frontend must keep this key server-side and proxy its requests. Do not put
the operator key in browser JavaScript, URLs, or local storage. Configure
cross-origin access using a comma-separated exact-origin allowlist
(`UATU_CORS_ORIGINS`), defaulting to no browser origins. CORS is not
authentication and never permits wildcard origins. Do not implement browser
sessions or per-person authorization in this issue. A later change may expose
consented public profiles only after the policy, takedown and deletion behavior
from issue #10 are accepted.

### Resource contract

- `GET /v1/persons` lists/searches tracked people with bounded cursor
  pagination; `POST /v1/persons` creates
  a person from a bounded set of supported identifiers and queues initial
  account resolution/collection. `GET /v1/persons/{id}` returns the profile
  and source coverage. `PATCH /v1/persons/{id}` adds or removes identifiers
  using auditable identity claims; removal updates derived associations but
  does not promise erasure of raw source payloads.
- `POST /v1/persons/{id}/sync` creates or coalesces durable collection work.
  Require an `Idempotency-Key`; a repeated key for the same request returns
  the same job, while reuse with different request parameters is a conflict.
  Return `202 Accepted`, a job identifier and a `Location`. `GET
  /v1/jobs/{id}` returns the authorized job status. GET routes never call an
  upstream service or enqueue work.
- `GET /v1/persons/{id}/activity` accepts required `from` and `to` RFC3339
  timestamps for a half-open `[from,to)` range, optional source and kind
  filters, and an opaque keyset cursor. Order results deterministically by
  occurrence time, event ID and role. `limit` defaults to 100 and is bounded
  at 500. Bind cursors to the person, filters and range that produced them;
  cursors are authenticated, and clients restart pagination if a cursor
  becomes invalid after key rotation.
- `GET /v1/persons/{id}/summary` returns computed counts grouped by year,
  source and kind. `GET /v1/persons/{id}/projects` returns distinct projects
  within an optional time range with bounded cursor pagination. Both are
  computed from stored events and counts; do not add materialized summaries
  without measurement.
- `GET /v1/persons/{id}/coverage` reports source coverage and gaps for a
  required half-open time range. Preserve the existing `/activity`,
  `/coverage` and `/counts` routes while clarifying and documenting their
  request and response contracts.
- Do not expose an identity-resolution endpoint, email addresses, raw identity
  keys, raw source payloads, or source credentials through this API. Resolving
  ambiguous identity evidence is an explicit conflict, never an automatic
  person merge.

### HTTP and schema contract

- Publish an OpenAPI 3 specification at `docs/openapi/v1.yaml`. Use JSON
  consistently, with success responses in a `data` envelope and pagination
  metadata alongside it. Errors use a stable JSON object with machine-readable
  `code` and safe human-readable `message`; never include SQL, upstream
  response bodies, credentials or private identity evidence.
- Use `400` for malformed input, `401` for missing/invalid authentication,
  `404` for absent resources, `409` for identifier conflicts or idempotency
  key reuse with different parameters, `413` for oversized request bodies,
  `429` for configured request limits, and `500` for unexpected server errors.
  Include a request ID in responses and structured logs.
- Bound JSON bodies to 64 KiB, reject unknown fields and trailing JSON values,
  validate path/query/body inputs, and cap page sizes and identity-list
  lengths. Do not accept arbitrary upstream URLs or unbounded identity lists.
- Use additive changes within `/v1`. A breaking change to route semantics,
  fields or cursor behavior requires a new API version; application release
  versioning does not relax the `/v1` contract.

## Consequences

All person information remains private to trusted API-key holders until a
separate privacy decision permits public reads. The static key makes the
initial API implementable without introducing an identity provider, but does
not support per-user permissions, key rotation without deployment, or safe
direct use from an untrusted browser. Deployments must provision the key,
protect it as a secret, and configure TLS at their ingress.

Public profile discovery, identity resolution, raw event inspection, account
sessions and fine-grained authorization remain out of scope. Identifier
removal changes current identity assignment and derived attribution but is not
a raw-evidence deletion or takedown guarantee. Implementing those guarantees
requires issue #10 and its own accepted privacy decision.

The existing `/v1` response shapes may need a pre-1.0 compatibility change to
adopt the documented envelopes and cursor contract. The frontend must consume
the OpenAPI contract rather than infer response shapes.

## Validation

- Obtain maintainer acceptance of this ADR before implementing the public
  contract. Implement it in a separate change linked to issue #6.
- Test that all `/v1` routes reject missing/incorrect keys, `/healthz` is safe
  without a key, comparisons do not leak credential values, and unconfigured
  CORS rejects cross-origin access.
- Test body-size, field, timestamp, filter, cursor, page-size and idempotency
  validation, including identifier conflicts and safe error responses.
- Verify GET handlers never call upstream services or enqueue work; verify
  sync requests return the same job for a repeated matching idempotency key
  and a conflict for mismatched reuse.
- Verify cursor pagination is deterministic with timestamp ties and across
  filters, and summaries/projects match stored timeline and aggregate data.
- Validate the OpenAPI document against handler behavior and example requests.
  All tests use local fixtures and isolated PostgreSQL, never a real upstream
  service or GitHub token.
- Run `gofmt -l .`, `go vet ./...`, `go test ./...` and `go test -race ./...`.

## References

- [ADR 0001: PostgreSQL and backend-only scope](0001-postgresql-backend-scope.md)
- [ADR 0004: Person, identity and event data model](0004-person-identity-event-model.md)
- [ADR 0005: Resumable bounded collection and normalization](0005-resumable-collection.md)
- [Issue #6: Publish the versioned backend API](https://github.com/project-uatu/backend/issues/6)
- [Issue #10: Identity administration and privacy lifecycle](https://github.com/project-uatu/backend/issues/10)
