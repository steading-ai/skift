# CI

Four workflows run here.

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

`make lint` reads the workflows too: actionlint checks their syntax, expressions and shell, and zizmor audits them for the mistakes that make a workflow exploitable, such as template injection, persisted credentials and over-broad permissions.

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

[actions-lock.yml](../.github/workflows/actions-lock.yml) runs on every
pull request and every push to `main`: `gh actions-lock --verify` fails
when [actions.lock](../.github/workflows/actions.lock), which pins the
commit behind every action the workflows use, no longer matches them or
pins a commit that no longer verifies upstream. `gh actions-lock` rewrites
it. Renovate ([renovate.json5](../renovate.json5)) bumps the actions and
regenerates the lockfile with them.

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
