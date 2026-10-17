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
creates a `vX.Y.Z` tag and GitHub Release, then publishes the container image
(see below). It does not change the Go toolchain version in `go.mod`.

The initial manifest baseline is `0.0.0` (not an existing release). Without a
previous release, Release Please would default to `1.0.0`, so
`release-please-config.json` sets `initial-version` to `0.1.0`. The first
release is therefore `v0.1.0` regardless of breaking changes in the history.
Conventional Commits determine subsequent versions: `feat:` bumps the minor
version, `fix:` bumps the patch, and `feat!:` or a `BREAKING CHANGE:` footer
marks a breaking change. Before 1.0, breaking changes bump the minor version;
after 1.0, they bump the major version. Release 1.0.0 only when deliberately
promoted, for example with a `Release-As: 1.0.0` commit footer. `docs:`, `chore:`, and `ci:` alone do not request a release. Use
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
- CI retains read-only permissions. The Release Please job does not check out or
  execute repository code and uses only the scoped release token.

## Container image

When Release Please creates a release, the `publish-image` job in the same
workflow builds the released tag and pushes it to `ghcr.io/project-uatu/backend`.
It runs only when `release_created` is true, so release-metadata changes and
ordinary pushes never publish an image.

- Platforms: `linux/amd64` and `linux/arm64`. The `arm64` image is built
  under QEMU emulation.
- Tags: the version (`0.1.0`), the minor version (`0.1`), the major version
  from `1.0.0` onwards (`1`), and `latest`. Tags never carry the `v` prefix.
- Provenance: the job attaches a signed GitHub build provenance attestation to
  the image digest. Verify an image with
  `gh attestation verify oci://ghcr.io/project-uatu/backend:<tag> --repo project-uatu/backend`.
- Permissions: the job uses the built-in `GITHUB_TOKEN` with `packages: write`,
  `id-token: write` and `attestations: write`. No registry secret is needed.

On the first publish, GitHub creates the package as private. In the package
settings, make it public and confirm that it is linked to this repository so
that anonymous pulls work.

To retry a failed publish, re-run the failed `publish-image` job from the same
workflow run. Pushing the tag again does not trigger a publish.
