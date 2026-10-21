# 0010: Bounded seeded-repository commit history

Status: Accepted

Date: 2026-10-21

Issue: [#9](https://github.com/project-uatu/backend/issues/9)

Prerequisites: accepted ADRs [0001](0001-postgresql-backend-scope.md),
[0004](0004-person-identity-event-model.md),
[0005](0005-resumable-collection.md),
[0006](0006-github-event-enumeration.md),
[0007](0007-versioned-person-api.md),
[0008](0008-kubernetes-delivery-contract.md) and
[0009](0009-gitlab-codeberg-connectors.md).

Decision authority: Accepted by the maintainer on 2026-10-21.

## Context

Issue #9 adds authored commits. Commits are not enumerable by person on any
supported forge, so history has to be read per repository from a bounded seed
set. ADR 0004 requires that upstream commit APIs are evaluated before cloning
and that any clone cache is disposable, bounded and approved in an ADR first.

The collector already stores a person's **known repositories** (ADR 0006 and
0009): public repositories where a stored issue, pull request, review or release
was observed. This ADR uses them as the seed set.

Constraints: PostgreSQL is the only canonical state; replicas are stateless and
failed fetches never overwrite stored data; the binary is static with a
distroless non-root image; only public data is stored; per-host quotas apply.

### What the upstream APIs provide

Checked against live responses on 2026-10-18.

**GitHub**

- `GET /repos/{owner}/{repo}/commits` accepts `author` (a login or an email),
  `since`, `until`, `sha` (a branch or commit to start from) and `per_page=100`.
  Without `sha` it lists the default branch only. Each item has the git author
  and committer (name, email, date), `parents`, and `author`, the linked GitHub
  user, which is null when the commit email is not linked to an account.
- `GET /repos/{owner}/{repo}/branches` lists branches, paginated. Listing
  commits per branch costs one scan per branch.
- The commit search API is limited to the default branch and has a separate,
  much smaller quota, so it is not used.
- GraphQL `history(author:)` works per ref and costs query points. It adds
  nothing over REST for the default branch.
- Authenticated REST quota is 5,000 requests per hour; unauthenticated is 60.

**GitLab.com**

- `GET /projects/:id/repository/commits` accepts `since`, `until`, `ref_name`,
  `all=true` (every ref) and `per_page`, with `x-next-page` pagination.
- Items expose `author_name`, `author_email` and committer fields only. There
  is no linked account or user ID, so attribution can use email only.
- `RateLimit-*` headers describe the applicable rate-limit bucket; they do
  not mean authenticated requests are unlimited. GitLab.com currently
  documents 2,000 authenticated API requests per minute per user and 500
  unauthenticated requests per minute per IP address, alongside
  endpoint-specific limits ([rate limits](https://docs.gitlab.com/user/gitlab_com/rate_limits/#current-rate-limits)).

**Codeberg (Forgejo)**

- `GET /repos/{owner}/{repo}/commits` accepts `sha`, `since`, `until`, `stat`,
  `verification` and `files` with `limit` and `x-hasmore` pagination. There is
  no author filter. Items carry `author` and `committer` user objects with a
  numeric `id` and `login` when the email maps to an account, plus the git
  author email.
- Quota is the IETF `ratelimit` header (2,000 per 10 minutes observed).

None of the three supports a person-scoped global commit listing, and none
exposes a trustworthy "every branch" view with authorship at low cost.

## Alternatives

| Option | Benefits | Costs |
|---|---|---|
| Upstream commit APIs per seeded repository (chosen) | No disk or CPU use, resumable by page, shares the existing quota and cursor machinery, works in a static distroless image | Limited to the default branch, no author filter on GitLab and Codeberg (the scan is bounded by `since` and a page budget), email-only attribution on GitLab |
| Pure-Go clone (for example go-git) for every repository | Full history, every branch, no API quota | Needs a writable bounded volume, clone and pack CPU and memory, pruning, a malformed-repository attack surface, long-running leases, and shallow clones cannot prove completeness |
| API first, clone only for large or unsupported repositories | Complete where it matters | Two code paths and a cache policy, for a benefit not yet demonstrated |
| Commit search API or GraphQL history | Possible author filtering | Default branch only, separate quotas, no added coverage |
| Explicit seed list from the API | Covers repositories without other activity | New API and privacy surface; a separate decision |

## Decision

Collect commits through the upstream APIs for the person's known repositories.
Do **not** clone in this change. A clone strategy needs its own accepted ADR
with the filesystem policy ADR 0004 requires; the clone-specific acceptance
items in #9 (pruning, restarts, malformed repositories, distroless
compatibility) then apply to that ADR.

### Seed set and budgets

- Seeds are the known public repositories of the person on that forge, ordered
  in a stable order by repository path so scans resume predictably, excluding
  forks. The default cap is 50 repositories per
  person per forge, configurable as `UATU_COMMIT_MAX_REPOS`. Repositories over
  the cap are recorded as a coverage gap, not silently dropped. Ordering by
  recent activity needs new tracking and is left to a later change.
- Each repository scan reads at most `UATU_COMMIT_MAX_PAGES` pages of 100
  commits (default 20, so 2,000 commits). GitHub filters by author on the
  server, so this budget rarely binds there. GitLab and Codeberg have no author
  filter, so the scan is bounded by `since` and the page budget; exceeding it
  is a gap for that repository and range.
- Every page is one upstream request reserved against the existing per-host
  quota bucket for the forge (ADR 0005). No CPU or disk budget is needed
  because nothing is cloned.
- Seeds are never discovered by crawling, and an explicit seed list is out of
  scope here.

### Branch coverage

Only the **default branch** is read. This keeps the cost at one scan per
repository and avoids counting unmerged commits. Commits that exist only on
other branches are outside the coverage scope, and the scope string says so:
`authored non-merge commits on the default branch of up to N known public
non-fork repositories`. Empty repositories and repositories the host refuses to
serve (404, 409, 451) are reported as gaps with the reason.

### Attribution

Attribution uses verified evidence only. A commit email is not an account
claim.

- **GitHub:** count a commit when its linked `author.id` equals the tracked
  account ID. A GitHub noreply email of the form `ID+login@users.noreply.github.com`
  also verifies the ID. An unlinked commit counts only when its email is an
  email identity already **assigned** to this person (ADR 0004).
- **Codeberg:** count a commit when the linked `author.id` equals the tracked
  account ID, or when its email is an assigned email identity of the person.
- **GitLab:** there is no linked account. Count a commit only when its author
  email is an assigned email identity of the person.
- The collector never creates identity claims from commit data, never matches
  names, and does not attribute commits from committer fields. Parse
  `Co-authored-by` trailers as attribution claims: credit a co-author only
  when the trailer email matches an email identity already assigned to that
  person under ADR 0004. Never resolve a co-author by trailer name or create
  an identity claim from a trailer. Represent verified co-authorship separately
  from primary authorship; do not count the same person's role twice for one
  commit. Unassigned or shared emails do not establish attribution.
- Merge commits (more than one parent) are not counted.

### Storage and normalization

- New sources `github_commits`, `gitlab_commits` and `forgejo_commits` have
  their own checkpoints, cursors and quota buckets, so a commit scan never
  blocks or is mistaken for the activity history.
- The raw event kind is `commit` with external ID `commit:{repository_id}:{sha}`.
  The payload holds only the SHA, repository path, repository URL, authored
  time, a merge flag and the attribution basis (`account_id`, `noreply` or
  `assigned_email`). It holds **no message, names or email addresses**.
- The normalizer emits one `commit_authored` action at the authored time. The
  event is linked to the person by the existing account or assigned identity
  rules, not by a stored email. A change to identity assignments re-derives
  links under ADR 0004 without refetching.
- `contribution_counts` from GitHub's `commits` metric remain separate
  aggregates. They are never added to individual commit events.
- Authored time is the commit's author date. It is untrusted and not used to
  advance watermarks; the scan window and checkpoint come from the job.

### Scheduling and compatibility

- A commit job for a forge is queued when that forge's history scan completes
  (so the seed set is current), using the existing idempotency key, and by
  `POST /v1/persons/{id}/sync`. The first scan is a backfill from the account
  creation date, or the per-forge floor date; later scans use the checkpoint
  with a seven-day overlap. Raw events are idempotent by external ID.
- Workers claim only sources they have a connector for, so replicas running the
  previous release ignore commit jobs during a rolling update.
- Failed pages never overwrite stored data. Interrupted scans resume from the
  persisted per-repository cursor.
- A boundary gap (for example the repository cap) stays visible in coverage but
  does not block later incremental scans, as in ADR 0009.

### Privacy

Only public repositories and public commit metadata are read. Deleting a person
removes their commit events with the rest of their data. Emails are used for
matching in memory and are not persisted by this connector.

## Consequences

- Commit history covers known repositories only, on the default branch, bounded
  by the repository and page caps. This is stated in every response that
  includes commits, and is not presented as complete history.
- GitLab and Codeberg scans cost more requests than GitHub because they cannot
  filter by author server-side.
- GitLab attribution depends on the person's assigned emails, so a person with
  no verified emails gets no GitLab commits.
- Co-authorship is credited only when a trailer email matches an already
  assigned identity. Contributions without trailers, with unassigned emails or
  with ambiguous/shared email identities are not attributed as co-authored.
- Commits on non-default branches, in forks, or in repositories with no other
  observed activity are absent until a later decision adds them.
- Static, distroless and non-root constraints are untouched because no clone
  cache, volume or new binary is introduced.

## Validation

- Unit tests with recorded sanitized fixtures for each host: author filtering,
  linked and unlinked authors, noreply emails, assigned and unassigned emails,
  verified and unverified `Co-authored-by` trailers, merge commits, forks,
  empty repositories, 404/409/451, and pagination.
- Tests that payloads and cursors contain no message, name or email.
- Tests that trailer names never establish identity, trailer parsing does not
  create identity claims, and the same person is not credited twice for one
  commit.
- Tests that over-cap repositories and over-budget scans become visible gaps and
  that a boundary gap does not force a new backfill.
- Store tests for source-filtered claims, checkpoint resume, retries, idempotent
  re-scans, and re-derivation after an identity assignment changes.
- Normalizer tests for `commit_authored` and for merge-commit exclusion.
- A kind-based smoke check that a mixed-version rollout leaves commit jobs
  queued until a capable replica claims them.
- The README documents the sources, the budgets, the coverage scope and the
  attribution rules.
