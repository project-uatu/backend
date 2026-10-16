# CI and release process

This document covers repository maintenance and release automation.

## CI

CI runs on pushes and pull requests. Go checks and the container build run only
when Go source, files under `cmd/` or `internal/`, dependencies, SQL/migrations,
test data, Docker inputs (`Dockerfile`, `.dockerignore`), lint configuration, or
the CI workflow change. Markdown files are excluded. Documentation, ADRs, and
release-only metadata changes skip those expensive checks.

Keep **`go`** as the required branch-protection check. It always runs and succeeds
only when change detection succeeds and the build/test job either succeeds or
is intentionally skipped. Do not require the conditional `test-build` job.

## Releases

[Release Please](https://github.com/googleapis/release-please) runs independently
on every push to `main`. It maintains a release PR updating `CHANGELOG.md` and
`.release-please-manifest.json` for the single root Go package. Merging that PR
creates a `vX.Y.Z` tag and GitHub Release; it does not publish a container image
or change the Go toolchain version in `go.mod`.

The initial manifest baseline is `0.0.0` (not an existing release). Conventional
Commits determine the first and subsequent versions: `feat:` bumps the minor
version, `fix:` bumps the patch, and `feat!:` or a `BREAKING CHANGE:` footer
marks a breaking change. Before 1.0, breaking changes bump the minor version;
after 1.0, they bump the major version. For example, the first feature release
is `v0.1.0`. `docs:`, `chore:`, and `ci:` alone do not request a release. Use
Conventional Commit PR titles when squash-merging, and preserve breaking-change
footers in the resulting commit message.

Repository setup:

- Create a GitHub App and install it on this repository only. Grant these
  repository permissions (no organization permissions or webhook subscriptions
  are needed):
  - **Contents: read and write** — release branches, version/changelog commits,
    tags, and GitHub Releases.
  - **Pull requests: read and write** — create and update release PRs.
  - **Issues: read and write** — manage release labels.
  - **Metadata: read-only** — automatically granted by GitHub.
- Set the Actions repository variable `RELEASE_APP_CLIENT_ID` to the App's
  client ID and the Actions repository secret `RELEASE_APP_PRIVATE_KEY` to its
  generated PEM private key. No client secret or stored installation token is
  needed; remove the old `RELEASE_PLEASE_TOKEN` secret if previously configured.
- The workflow mints a short-lived installation token before Release Please,
  explicitly scoped to this repository and the three write permissions above.
  The token action revokes it when the job finishes.
- An App token is intentional: PRs created with the built-in `GITHUB_TOKEN`
  do not trigger CI, which would leave the required `go` check pending.
  Release PRs created with this token run the lightweight check and skip
  Go/container work unless they also contain code changes.
- CI retains read-only permissions; the release workflow does not check out or
  execute repository code and uses only the scoped release token.

If image publishing is added later, gate it on
`steps.release.outputs.release_created == 'true'` in the release workflow and
build the released tag, rather than rebuilding on release-metadata changes or
relying on a separate tag-triggered workflow.
