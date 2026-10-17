# 0006: GitHub event enumeration and coverage strategy

Status: Proposed

Date: 2026-10-17

Issue: [#5](https://github.com/project-uatu/backend/issues/5)

Prerequisites: accepted ADRs [0001](0001-postgresql-backend-scope.md),
[0004](0004-person-identity-event-model.md) and
[0005](0005-resumable-collection.md).

Decision authority: Maintainer acceptance is required before implementation.

## Context

Issue #5 replaces the temporary owned-repository snapshot with a person-centric
GitHub history: authored issues and pull requests, pull request reviews, and
releases. The API must support multi-year history without claiming completeness
where GitHub does not provide a complete enumeration path. Identity must also
survive login changes, and source-provided contribution totals must not be
presented as individual events.

GitHub's REST Search API returns at most 1,000 results per query, has a
separate authenticated limit of 30 search requests per minute, and can report
incomplete results when a query times out. Its issue and pull request search
supports `author`, `reviewed-by`, `created` and `updated` qualifiers. Normal
REST pagination allows up to 100 results per page. Review records are listed
per pull request, while release records are listed per repository; there is
no global release-by-author endpoint. The user activity stream is not a
history source: it contains at most 300 events from the last 30 days.

These limits rule out a single unbounded search, activity-feed-only
collection, or scanning every public repository. The release endpoint in
particular cannot discover every repository where a person may have published
a release.

## Alternatives

| Option | Advantages | Costs |
|---|---|---|
| Use GraphQL contribution aggregates as the event source | Compact daily/type totals and repository summaries | Does not provide individual source records for the timeline; cannot enumerate release authors |
| Use REST Search and paginate every matching result | Returns individual issue and pull request records; supports author, reviewer and date qualifiers | Search result cap and rate limits require bounded partitioning; review search returns candidate PRs, not review records |
| Use the user event stream as the history source | Simple actor-oriented events | Only 30 days and 300 events; cannot establish multi-year coverage |
| Enumerate every public repository and inspect all activity | Could discover releases without a prior person/repository relationship | Unbounded, repository-centric crawling contrary to the project scope and API budget |
| Use REST Search for person activity and enumerate releases only in known public repositories | Bounded and person-seeded; exposes the release limitation honestly | Cannot claim account-wide release completeness for repositories not otherwise discovered |

## Decision

### Identity and public-data boundary

- Resolve each supplied GitHub login with `GET /users/{username}` and retain
  the REST numeric `id` and `node_id` as the canonical account identifiers.
  Keep the current login as a query locator, not as the identity key.
- Search requests use the current login because GitHub search qualifiers
  require it. Validate each returned actor's numeric ID against the stored
  account ID before attributing the record. Refresh the login from the stable
  GraphQL node ID when available; do not create a new person on a login change.
  Automatic identity merges and ambiguous rename recovery remain in issue #10.
- Collect only public repository and profile data. A credential must not grant
  access to private content for this collector, and repository visibility
  must be checked before persistence. Never store or log the credential.

### Authored issues and pull requests

- Use the REST Search issues and pull requests endpoint with separate
  `is:issue author:{login}` and `is:pr author:{login}` queries.
- Backfill by UTC calendar-date ranges using `created:`. Begin at the account's
  creation date where available; treat the range end as exclusive in Uatu's
  coverage records, even though GitHub's date qualifier is inclusive.
- Split a range into smaller ranges when `total_count` reaches the 1,000-result
  cap or `incomplete_results` is true. Retry transient query failures with
  bounded backoff. If a single day remains capped or incomplete after bounded
  retries, record a coverage gap and continue with other ranges; do not mark
  that interval complete.
- Request at most 100 results per page and persist page progress through the
  existing durable job cursor. Use the returned pagination links and
  `incomplete_results` field; do not infer completeness from an empty page.
- Use `updated:` queries with a configured overlap for incremental refreshes
  so old issues and pull requests changed since the last sync are re-read.
  Search results are validated against the canonical account ID. Store one raw
  record per repository ID and issue/pull request number, updating an existing
  record idempotently.
- For pull requests, fetch the pull request resource when needed to capture
  merge timestamps not present in issue search results. Normalize only
  source-backed lifecycle milestones available from the returned object;
  do not invent intermediate reopen or state-change events.

### Pull request reviews

- Discover candidate pull requests with `is:pr reviewed-by:{login}` and the
  same bounded `created:` partitions, pagination, cap handling and coverage
  gaps used for authored records.
- For each candidate pull request, page through
  `GET /repos/{owner}/{repo}/pulls/{pull_number}/reviews`. Persist a review
  only when its reviewer's numeric account ID matches the tracked account and
  it has a source timestamp. Use the immutable review ID as the event
  identifier and `submitted_at` as its occurrence time.
- On incremental sync, use an overlapping `updated:` search to rediscover
  candidate pull requests, then re-read their paginated review lists and
  upsert reviews by ID. A failure to finish candidate discovery or any review
  list leaves the affected coverage incomplete.

### Releases and known repositories

- Build a set of known public repositories from repositories in the person's
  authored issue/pull request and review records, plus their paginated public
  owned-repository list. Keep this set as internal collection state; it does
  not turn repository names into person contribution events or public API
  output.
- For each known repository, page through its REST releases endpoint and
  retain releases whose author's numeric account ID matches the tracked
  account. Use the release ID as the event identifier and `published_at` as
  the occurrence time. Re-scan recent pages with overlap during incremental
  sync and periodically re-scan a repository's full release list to detect
  corrections to older records.
- The public user event stream may be used as a hint to discover a recently
  active repository, but never as a complete release source. It is limited to
  300 events from the past 30 days.
- Release coverage is explicitly **known repositories only**. Because GitHub
  exposes no global release-by-author search, Uatu must not report
  account-wide release coverage as complete. Repositories not discovered by
  the person-scoped sources remain an acknowledged gap.

### Aggregate counts, quotas and provenance

- Use GraphQL `User.contributionsCollection` only for source-provided
  aggregate counts, queried in bounded calendar-year windows where useful.
  Store those observations in `contribution_counts`; never convert them into
  normalized events or add them to event counts. A difference between
  aggregates and enumerated events is not itself evidence that either source
  is complete.
- Keep REST search, REST core and GraphQL quota state separate by credential
  and resource. Read rate-limit headers on responses and honor `Retry-After`,
  reset times, secondary limits and cancellation. GitHub currently documents
  5,000 GraphQL points per hour for a user and a separate REST search limit;
  the implementation must use response-reported state rather than hard-code
  a single global quota.
- Persist query range, pages, result counts, partial/incomplete indicators,
  retries and known gaps in the existing sync report and coverage records.
  Use stable source IDs so overlapping scans and repeated syncs are
  idempotent.

## Consequences

REST Search supplies individual authored records, while reviews require a
second, repository-scoped enumeration step. Bounded date partitions preserve
progress while making search caps visible as gaps. The approach remains
person-seeded and does not crawl all repositories.

GitHub does not expose enough information to guarantee complete, account-wide
release history. Release records are complete only for the known public
repositories that were fully paginated; the API and coverage model must show
that scope. If account-wide releases are a product requirement, the source
strategy must be revisited rather than silently weakening the coverage claim.

The strategy uses both REST and GraphQL but stores their raw records through
the existing event contract and stores aggregate counts separately. It
requires per-resource quota coordination, bounded query cursors and explicit
failure reporting. It does not change the normalized event schema from ADR
0004 or the worker semantics from ADR 0005.

## Validation

- Use sanitized fixtures to test login-to-ID resolution, account-ID
  verification, issue/PR pagination, search partitioning at the result cap,
  incomplete results and irreducible daily gaps.
- Test review discovery and per-PR review pagination, reviewer ID filtering,
  stable review IDs and `submitted_at` ordering.
- Test repository-scoped release pagination, actor filtering, overlap,
  idempotent upserts and explicit known-repository coverage.
- Test that aggregate counts persist separately from normalized events and
  that no aggregate is converted into a fabricated event.
- Test REST search, REST core and GraphQL quota parsing, reset/secondary
  limit delays, cancellation and sanitized errors without network access or a
  real `GITHUB_TOKEN`.
- Against isolated PostgreSQL, verify initial backfill, incremental overlap,
  restart from stored cursors, repeated-sync idempotency and visible gaps.
- Run `gofmt -l .`, `go vet ./...`, `go test ./...` and `go test -race ./...`.

## Upstream documentation reviewed

Reviewed on 2026-10-17:

- [REST Search API limits and behavior](https://docs.github.com/en/rest/search/search)
- [Issue and pull request search qualifiers](https://docs.github.com/en/search-github/searching-on-github/searching-issues-and-pull-requests)
- [REST pagination](https://docs.github.com/en/rest/using-the-rest-api/using-pagination-in-the-rest-api)
- [List pull request reviews](https://docs.github.com/en/rest/pulls/reviews#list-reviews-for-a-pull-request)
- [List releases](https://docs.github.com/en/rest/releases/releases#list-releases)
- [User events](https://docs.github.com/en/rest/activity/events#list-public-events-for-a-user)
- [Get a user](https://docs.github.com/en/rest/users/users#get-a-user)
- [GraphQL User object](https://docs.github.com/en/graphql/reference/objects#user)
- [GraphQL rate and query limits](https://docs.github.com/en/graphql/overview/rate-limits-and-query-limits-for-the-graphql-api)
