# 0009: GitLab.com and Codeberg connectors

Status: Proposed

Date: 2026-10-18

Issue: [#8](https://github.com/project-uatu/backend/issues/8)

Prerequisites: accepted ADRs [0001](0001-postgresql-backend-scope.md),
[0004](0004-person-identity-event-model.md),
[0005](0005-resumable-collection.md),
[0006](0006-github-event-enumeration.md),
[0007](0007-versioned-person-api.md) and
[0008](0008-kubernetes-delivery-contract.md).

Decision authority: the maintainer must accept this ADR before implementation.

## Context

Issue #8 adds public contribution history from GitLab.com and Codeberg. It
must reuse the bounded connector/report contract, add no source-specific core
schema, keep forge instances distinct, respect per-host quotas and the
public/private boundary, and state history gaps.

Codeberg runs Forgejo. Both forges are checked below against their public
documentation and live responses on 2026-10-18.

### What the upstream APIs provide

**GitLab.com**

- `GET /api/v4/users?username=` resolves a login to a numeric user ID.
- `GET /api/v4/issues` and `GET /api/v4/merge_requests` accept `scope=all`,
  `author_id`, `created_after`, `created_before` and `updated_after`, so
  authored records can be enumerated globally by person.
- `GET /api/v4/users/:id/events` lists contribution events (`created`,
  `merged`, `approved`, `commented`, `pushed`, ...) with `after`/`before`
  date filters. GitLab documents that this API does not return epic or merge
  request events in full and that the contribution calendar covers only the
  last 12 months. It is therefore not a complete history source.
- Responses carry `RateLimit-Limit`, `RateLimit-Remaining`, `RateLimit-Reset`
  and `RateLimit-Name` headers. Unauthenticated requests were observed at
  500 per minute.
- Releases are listed per project; there is no global release-by-author
  endpoint.

**Codeberg (Forgejo)**

- `GET /api/v1/users/{username}` resolves a login to a numeric ID and
  creation date.
- `GET /api/v1/repos/issues/search` filters (`created`, `reviewed`,
  `assigned`, ...) are relative to the **authenticated** user. It cannot list
  another person's issues and pull requests. There is no person-scoped global
  search.
- `GET /api/v1/users/{username}/activities/feeds` lists a user's public
  activity with `only-performed-by`, a `date` filter and pagination. Operation
  types include `create_issue`, `create_pull_request`, `merge_pull_request`,
  `approve_pull_request`, `reject_pull_request` and `publish_release`. Deep
  pages were served, but retention is an instance setting and is not
  advertised.
- Per-repository `GET /repos/{owner}/{repo}/issues?created_by=` and
  `.../releases` exist, so authored records can be enumerated in a known
  repository.
- `GET /api/v1/users/{username}/heatmap` returns daily contribution counts.
- Rate limits are reported as `RateLimit` and `RateLimit-Policy` headers
  (observed `"baseline";q=2000;w=600`).
- Feed items embed the acting user, including a noreply email field. That
  field must not be stored.

### Gaps in the current implementation

- `github_accounts` and `github_known_repositories` are GitHub-named tables,
  and `store` special-cases `SourceGitHubHistory` when enqueueing and
  claiming jobs. A second forge would need a second pair of tables.
- `POST /v1/persons` and the identifier endpoints accept only a GitHub login
  and emails.
- The HTTP client sets no `User-Agent`, and each connector would otherwise
  duplicate response-size, rate-limit and redaction handling.

## Alternatives

| Option | Advantages | Costs |
|---|---|---|
| Add `gitlab_accounts` and `forgejo_accounts` tables | Mirrors GitHub exactly; no GitHub change | One migration per forge; contradicts the acceptance criterion and does not scale to Gerrit (#11) |
| Generalize the account and known-repository tables once, then add sources without migrations | Meets the criterion; instance scoping already exists in every key | One compatibility migration, and a rolling-update window that must be handled |
| Store account metadata only in `identity_keys` | No new table | No place for the current login, node ID or creation date used by incremental scans |
| Treat both forges as GitHub-like and use only global search | Single connector shape | Forgejo has no person-scoped global search; GitLab has no review enumeration |
| Use only each forge's user event stream | Uniform and cheap | Retention and the documented GitLab limits make it incomplete for multi-year history |
| Allow arbitrary self-hosted instances now | Matches ADR 0004 model | Requires an outbound allowlist and SSRF controls; deferred to #12 |

## Decision

### Forge instances and identity

- Add two enabled instances: `(gitlab, https://gitlab.com)` and
  `(forgejo, https://codeberg.org)`. The provider names the software family;
  the base URL identifies the instance. Self-hosted instances are out of
  scope here and need their own accepted decision on host allowlisting.
- The numeric account ID is the identity key, scoped to its forge instance.
  Logins are query locators and are refreshed from the stable ID on every
  sync. A person who is `alice` on GitLab.com and `alice` on Codeberg has two
  unrelated, instance-scoped accounts; they are linked only by an explicit
  person assignment (ADR 0004, #10).
- Raw event external IDs are unique within a forge instance and prefixed by
  type, for example `issue:{project_id}:{iid}`, `merge_request:{project_id}:{iid}`
  and `activity:{feed_id}`. Deduplication is `(forge_instance_id, external_id)`
  as it is today.
- `identity.Normalize` already accepts `gitlab` and `forgejo` login providers.

### One-time generalization of source state

- One migration adds `source_accounts` (forge instance, person, account ID,
  login, optional node ID, optional account creation time) and
  `known_repositories` (replacing `github_known_repositories`), backfilled
  from the GitHub tables. After it, a new source needs no core migration.
- `store` replaces its `SourceGitHubHistory` special cases with a small
  per-source profile (source key, provider, whether onboarding must resolve
  an account). Source keys are `gitlab_history` and `forgejo_history`.
- Rolling updates (ADR 0008) must keep working. This release reads the new
  tables and also writes the GitHub tables; a later release drops them in a
  contract migration. Both steps are covered by tests.

### Shared client behavior

- A small `internal/forgehttp` helper provides the shared transport: a
  descriptive `User-Agent` (`uatu/<version> (+https://github.com/project-uatu/backend)`),
  bounded response size, timeouts, rate-limit header parsing, `Retry-After`,
  and error redaction. GitLab and Forgejo clients use it. The GitHub client
  adopts it for the `User-Agent` only; its search logic is unchanged.
- Quota buckets include the host and resource, for example
  `gitlab/gitlab.com/api` and `forgejo/codeberg.org/api`, and are reserved
  through the existing durable limiter. Limits come from response headers,
  never from constants. Requests cancel promptly and a 429 defers the job.
- Tokens are optional, read-only and per forge: `GITLAB_TOKEN` and
  `CODEBERG_TOKEN`. They are never logged or stored. Collection works without
  them at lower limits.
- Only public data is persisted. A token must not widen visibility:
  repository or project visibility is checked from the response when present
  and otherwise fetched, before persisting a record. Private or unknown
  visibility is skipped and not logged with content.

### GitLab.com coverage

- Resolve the user by username and keep the numeric ID. Start backfill at the
  account creation date when exposed, otherwise at a documented per-forge
  floor date.
- Authored issues and merge requests: `GET /issues` and `GET /merge_requests`
  with `scope=all&author_id=`, bounded UTC `created_after`/`created_before`
  partitions, keyset pagination where documented and otherwise offset
  pagination with the same split-on-cap and gap rules as ADR 0006. Incremental
  syncs use `updated_after` with overlap.
- Lifecycle milestones come from `created_at`, `closed_at` and `merged_at` on
  the returned object. No intermediate state changes are invented.
- Releases: per known public project, filtered by the author's numeric ID.
  Coverage is **known projects only**, as for GitHub.
- Reviews: `approved` events from the user events API, labeled with coverage
  scope `recent_events`. If sanitized fixtures show the endpoint does not
  return them (see the documented limitation), reviews are reported as an
  explicit unsupported source gap and not as complete.

### Codeberg coverage

- Resolve the user by username and keep the numeric ID and creation date.
- Primary enumeration is the user activity feed with `only-performed-by=true`,
  paged and partitioned by `date`. It supplies issue and pull request
  creation, merges, approvals/rejections and releases. Incremental syncs
  re-read a bounded overlap and upsert by feed ID.
- Because the feed depth is instance-defined, the earliest reachable entry is
  recorded as the coverage boundary. History before it is a stated gap unless
  filled from known repositories.
- Known repositories (from feed items and the user's public repositories)
  are enumerated with `issues?created_by=`, pull request `poster` filtering
  and `releases` to backfill authored records the feed no longer returns.
  Coverage for this part is **known repositories only**.
- Feed items are reduced to the fields the normalizer needs. The embedded
  user email and unrelated profile fields are dropped before persistence.
- The heatmap is stored as aggregate counts in `contribution_counts` only,
  never as events.

### Normalization

Both sources map into the existing normalized kinds (`issue_opened`,
`issue_closed`, `pull_request_opened`, `pull_request_merged`,
`pull_request_closed`, `pull_request_reviewed`, `release_published`) with
source-specific pure handlers. No event schema change is required.

### Person API

- `POST /v1/persons` and the identifier add/remove endpoints accept an
  optional `forge` field (`github.com` by default, `gitlab.com`,
  `codeberg.org`). Existing requests keep their meaning, so this is an
  additive change within `/v1`.
- Adding a login for a forge enqueues the matching onboarding job. Coverage
  responses already report per-source status and gap counts; they also carry
  the coverage scope so clients can see `known_repositories` and
  `recent_events`.
- Configuration adds `GITLAB_TOKEN` and `CODEBERG_TOKEN` (collect only) and
  per-forge enablement. The README documents them.

## Consequences

- No per-forge schema from this point on. The cost is one compatibility
  migration and a two-release contract window.
- Neither forge can claim complete, account-wide history. GitLab review and
  release coverage and Codeberg pre-feed history are explicit and visible in
  coverage records, consistent with ADR 0006.
- Codeberg depends on the activity feed, whose retention Codeberg controls;
  a retention change shrinks coverage, never silently corrupts it.
- Rate limiting is per host, so a slow forge cannot starve the others.
- Self-hosted instances, commit history (#9) and Gerrit (#11) are not
  included.

## Validation

- Sanitized fixtures, with no network and no real tokens, cover user
  resolution, ID verification, pagination (including keyset and feed paging),
  the result-cap split, incomplete pages, irreducible gaps, redaction and
  dropped feed email fields.
- Tests cover visibility checks that skip private or unknown records, quota
  header parsing for both header styles, 429 and `Retry-After` handling,
  cancellation, and the `User-Agent`.
- Tests show that identical logins on two forges, or on GitLab.com and a
  second GitLab instance, stay distinct and that repeated and overlapping
  syncs are idempotent.
- Against isolated PostgreSQL, tests cover the migration backfill, N/N-1
  compatibility, restart from stored cursors and visible coverage scopes.
- Run `gofmt -l .`, `go vet ./...`, `go test ./...`, `go test -race ./...`
  and `golangci-lint run`.

## Upstream documentation reviewed

Reviewed on 2026-10-18:

- [GitLab Events API](https://docs.gitlab.com/api/events/)
- [GitLab contributions calendar](https://docs.gitlab.com/user/profile/contributions_calendar/)
- [Forgejo API usage](https://forgejo.org/docs/latest/user/api/usage/)
- Codeberg's published OpenAPI document at `https://codeberg.org/swagger.v1.json`
