# CI

Seven workflows live here.

[check.yml](../.github/workflows/check.yml) runs on every pull request and
every push to `main`: `make lint`, then `make cover`. That is every spec,
including the ones that need PostgreSQL (through testcontainers on the
runner's Docker) and the acceptance scenarios against the built binary,
with the floor in `coverage.baseline` enforced. It is the gate the
pre-commit hook applies locally, plus what the hook leaves out for speed.
A second job in the same workflow runs `make test-race`, every spec under
the race detector, and `make fuzz`, each fuzz target for a short while;
it runs beside the first rather than after it. Tools come from
`mise.toml` through `jdx/mise-action`, so the versions are the ones a
developer runs.

`make lint` reads the workflows too, with [jactionlint](https://jactionlint.jdx.dev): their syntax, expressions and shell, and the mistakes that make a workflow exploitable, such as template injection, persisted credentials and over-broad permissions. It checks `actions.lock` against the actions the workflows use as well.

[publish.yml](../.github/workflows/publish.yml) runs on every push to
`main`: it cross-compiles `skift` for linux/amd64 and linux/arm64 (the same
`dist/skift-linux-%` build the Makefile uses for the rpm) and pushes a
multi-arch image to GHCR, built from
[packaging/Containerfile](../packaging/Containerfile). It does not run on
pull requests — it publishes, it does not check anything.

[site.yml](../.github/workflows/site.yml) builds
[skift.stead.ing](https://skift.stead.ing/) from this repository and
publishes it to GitHub Pages. [site.yaml](../site.yaml) writes the front
page; `README.md` and every Markdown file under `docs/` are the pages
under `/docs/`. It runs when any of those change: a pull request builds
the site without publishing it, and a push to `main` publishes. The site's
code is not here. It is shared by every Steading project and comes from
[steading-ai/stead.ing](https://github.com/steading-ai/stead.ing), whose
README documents `site.yaml`.

[actions-lock.yml](../.github/workflows/actions-lock.yml) runs on every pull request and every push to `main`: `gh actions-lock --verify` fails when [actions.lock](../.github/workflows/actions.lock), which pins the commit behind every action the workflows use, no longer matches them or pins a commit that no longer verifies upstream. `gh actions-lock` rewrites it, and Renovate regenerates it with each action it updates.

[release.yml](../.github/workflows/release.yml) runs when a `v*` tag is pushed. It builds the archives, SBOMs and checksums with `make release`, attests each archive, puts every file on a draft release, signs a packslip manifest for it, and only then publishes, because a published release is immutable. [ADR-011](adrs/011-releases-are-immutable-and-attested.md) has the reasons for that order.

[verify-release.yml](../.github/workflows/verify-release.yml) checks a published release from the outside: GitHub's attestation of the release, the checksums, each archive's provenance and SBOM attestations, and an install with mise on linux/amd64, linux/arm64 and darwin/arm64. `release.yml` calls it last, and it can be dispatched for any tag.

[release-notes.yml](../.github/workflows/release-notes.yml) writes a published release's notes with [communiqué](https://communique.jdx.dev), configured by [communique.toml](../communique.toml). The repository stores no model credential: the job exchanges its GitHub identity for a short-lived Anthropic token by [workload identity federation](https://platform.claude.com/docs/en/manage-claude/workload-identity-federation), naming the identifiers the `release` environment holds as variables. Another workflow can call it with a tag, and it can be dispatched for any release; with `dry_run` it writes the notes to the job summary and leaves the release as it is.

## Dependency updates

Renovate ([renovate.json5](../renovate.json5)) opens a pull request when something the repository pins has a newer release:

- the Go modules `go.mod` requires directly. A module marked `// indirect` moves when a direct one needs a newer version of it, or when a fix for a published vulnerability in it exists. `go mod tidy` runs on every such update;
- the tools in `mise.toml`, with `mise.lock`;
- the Go toolchain, which is pinned twice: in `mise.toml`, and as the `go` directive of `go.mod`, which `publish.yml` builds with. One pull request moves both;
- the PostgreSQL image, also pinned twice: in `compose.yaml` and in `internal/testsupport/postgres.go`. One pull request moves both, and the digest stays;
- the hk release `hk.pkl` names, with the hk in `mise.toml`;
- the GitHub Actions the workflows use, with `actions.lock`, and the `gh-actions-lock` release `actions-lock.yml` installs.

It reads nothing under `archive/`.

An update merges by itself once every check on its pull request has passed. That is `check.yml` and `actions-lock.yml`, and `site.yml` when the change touches what the site is built from. An update that fails a check waits for a person: a buf or sqlc release that changes the generated code fails `make lint` until someone runs `make generate` on the branch. Updates to hk and to gremlins always wait for a person, because no check runs the git hooks or `make mutate`.

A release from the Go module proxy or from GitHub is proposed three days after it is published. A new image digest and a vulnerability fix are proposed at once. The cooldown and the merge rule come from the preset the configuration extends, [ivy/renovate-config](https://github.com/ivy/renovate-config).

## The image

- **Name:** `ghcr.io/steading-ai/skift`
- **Tags:** the full commit SHA, one per push — immutable, pinnable — and
  `main`, which moves to whatever the latest push produced.
- **Contents:** `skift` at `/usr/local/bin/skift`, on a `scratch` base. Nothing
  else — no shell, no entrypoint. `podman run --rm
  ghcr.io/steading-ai/skift:<sha> skift version` runs the binary itself as
  the container's command.
- **Auth to build and push:** the workflow's own `GITHUB_TOKEN`, granted
  `packages: write`. No other secret is involved.

## Reading it from another repository

The package is public, as the repository is, so any workflow or machine pulls `ghcr.io/steading-ai/skift` with no login and no grant.

## Releases

What a release contains, who builds each part, and how it is signed and verified are governed by [ADR-011](adrs/011-releases-are-immutable-and-attested.md). The image this page describes is the edge feed built from `main`; release artifacts are a separate matter decided there.
