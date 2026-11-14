# CI

Eight workflows live here.

[check.yml](../.github/workflows/check.yml) runs on every pull request and every push to `main`, as three jobs side by side. `check` runs `make lint`, then `make cover`: every spec, including the ones that need PostgreSQL (through testcontainers on the runner's Docker) and the acceptance scenarios against the built binary, with the floor in `coverage.baseline` enforced. It is the gate the pre-commit hook applies locally, plus what the hook leaves out for speed. `race` runs `make test-race`, every spec under the race detector. `fuzz` runs `make fuzz`, each fuzz target for a short while. Tools come from `mise.toml` through `jdx/mise-action`, so the versions are the ones a developer runs.

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

[release.yml](../.github/workflows/release.yml) runs when a `v*` tag is pushed. It builds the archives, SBOMs and checksums with `make release` and attests each archive; has `release-image.yml` push the image by digest; renders `docker-compose.yaml` pinned to that digest; puts every file on a draft release; signs a packslip manifest for it; and only then publishes, because a published release is immutable. After publication it tags the image with what `make image-tags` prints, has `release-notes.yml` write the notes, and has `verify-release.yml` check the result. [ADR-011](adrs/011-releases-are-immutable-and-attested.md) has the reasons for that order, and [How to cut a release](releasing.md) is the maintainer's guide to running it.

[release-image.yml](../.github/workflows/release-image.yml) runs only when `release.yml` calls it, with a version and the name of a workflow artifact holding the linux binaries `make release` built. It assembles the multi-arch image from them without compiling anything, pushes it to GHCR by digest and under no tag, checks that the pushed image reports the version, attests its provenance and its CycloneDX SBOM against the index digest, in the registry as well, and hands the digest back. Tagging is its caller's, after the release is published.

[verify-release.yml](../.github/workflows/verify-release.yml) checks a published release from the outside: GitHub's attestation of the release, the checksums, each archive's provenance and SBOM attestations, the image's attestations and that the compose file pins the digest the image's tags resolve to, and an install with mise on linux/amd64, linux/arm64 and darwin/arm64. A release from before the image was released has no compose file, and its image checks are skipped. `release.yml` calls it last, and it can be dispatched for any tag. [How to verify a release](verifying-a-release.md) shows the same checks by hand.

[release-notes.yml](../.github/workflows/release-notes.yml) writes a published release's notes with [communiqué](https://communique.jdx.dev), configured by [communique.toml](../communique.toml). The repository stores no model credential: the job exchanges its GitHub identity for a short-lived Anthropic token by [workload identity federation](https://platform.claude.com/docs/en/manage-claude/workload-identity-federation), naming the identifiers the `release` environment holds as variables. `release.yml` calls it with the tag it published, and it can be dispatched for any release; with `dry_run` it writes the notes to the job summary and leaves the release as it is.

## Dependency updates

Renovate ([renovate.json5](../renovate.json5)) opens a pull request when something the repository pins has a newer release:

- the Go modules `go.mod` requires directly. A module marked `// indirect` moves when a direct one needs a newer version of it, or when a fix for a published vulnerability in it exists. `go mod tidy` runs on every such update;
- the tools in `mise.toml`, with `mise.lock`;
- the Go toolchain, which is pinned twice: in `mise.toml`, and as the `go` directive of `go.mod`, which `publish.yml` builds with. One pull request moves both;
- the PostgreSQL image, also pinned twice: in `compose.yaml` and in `internal/testsupport/postgres.go`. One pull request moves both, and the digest stays;
- the hk release `hk.pkl` names, with the hk in `mise.toml`;
- the GitHub Actions the workflows use, with `actions.lock`, and the `gh-actions-lock` release `actions-lock.yml` installs.

It reads nothing under `archive/`.

An update merges by itself once every check on its pull request has passed. That is `check.yml` and `actions-lock.yml`, and `site.yml` when the change touches what the site is built from. An update that fails a check waits for a person: a buf or sqlc release that changes the generated code fails `make lint` until someone runs `make generate` on the branch. `make lint` validates `hk.pkl` against the hk that is installed, so an hk release that no longer reads the hook configuration fails its pull request too. Nothing runs gremlins in a check: a release of it that breaks `make mutate` merges, and shows the next time someone runs it.

A release from the Go module proxy or from GitHub is proposed three days after it is published. A new image digest and a vulnerability fix are proposed at once. The cooldown and the merge rule come from the preset the configuration extends, [ivy/renovate-config](https://github.com/ivy/renovate-config).

## Runners

`check.yml` runs on [Blacksmith](https://docs.blacksmith.sh) runners: `check` and `race` on 8 vCPUs, `fuzz` on 4. Measured on this repository, `check` takes about 5m45s, 3m40s and 3m15s on 4, 8 and 16 vCPUs, and `race` with the fuzz targets about 8m45s, 7m15s and 6m; on GitHub's runners they took about 10 and 13 to 20 minutes. Past 8 vCPUs a job costs twice as much for under a minute less, because the time goes on running the specs against PostgreSQL, not on compiling. The fuzz targets run for a fixed time on any machine, so they have a small runner to themselves.

Blacksmith keeps the Go build cache and the container images a job pulls between runs. The workflow adds its cached checkout and [sticky disks](https://docs.blacksmith.sh/blacksmith-caching/dependencies-sticky-disks) for the mise tools, the Go module cache and golangci-lint's cache. Go's test result cache stays off: the database specs are not hermetic.

Every other workflow runs on GitHub's own runners, and for `release.yml`, `release-image.yml`, `verify-release.yml`, `release-notes.yml` and `publish.yml` that is deliberate. What they build is published, so it is built from no cache another run could have written, and an attestation of a release then says it was built on a GitHub-hosted runner. A spec holds them there.

## The image

- **Name:** `ghcr.io/steading-ai/skift`
- **Edge tags**, from `publish.yml`: the full commit SHA, one per push to `main`, which never moves, and `main`, which moves to whatever the latest push produced. An edge image is not attested.
- **Release tags**, from `release.yml`: `X.Y.Z`, which never moves, because the workflow refuses a version that is already tagged. A stable release also moves `X.Y`, `X` from 1.0 on, and `latest`, each only forward, so a patch to an older line moves its own `X.Y` and nothing else. A prerelease such as `X.Y.Z-rc.1` gets its exact tag alone. `make image-tags VERSION=X.Y.Z` prints the tags a version takes. A release's image is attested, and [How to verify a release](verifying-a-release.md#check-the-image) checks it.
- **Digest pin:** a reference that carries a digest names one image whatever its tags do afterwards. The compose file a release carries pins its image this way:

  ```yaml
  image: ghcr.io/steading-ai/skift:<version>@sha256:<digest>
  ```

- **Contents:** `skift` at `/usr/local/bin/skift`, on a `scratch` base, and two empty directories it owns: `/run/skift`, where `skift serve` binds its socket, and `/var/lib/skift`, for the tailnet node's state. Nothing else — no shell, no entrypoint, no command. `podman run --rm ghcr.io/steading-ai/skift:<sha> skift version` runs the binary itself as the container's command.
- **User:** uid and gid 65532, by number; the image has no `/etc/passwd`. Both directories are that user's, mode 0750, and a new volume mounted over either starts out the same way.
- **TLS:** the image has no CA bundle. The binary carries Mozilla's roots from [`golang.org/x/crypto/x509roots/fallback`](https://pkg.go.dev/golang.org/x/crypto/x509roots/fallback) and uses them only where the system has none, as here.
- **Auth to build and push:** the workflow's own `GITHUB_TOKEN`, granted
  `packages: write`. No other secret is involved.

## Reading it from another repository

The package is public, as the repository is, so any workflow or machine pulls `ghcr.io/steading-ai/skift` with no login and no grant.

## Releases

What a release contains, who builds each part, and how it is signed are governed by [ADR-011](adrs/011-releases-are-immutable-and-attested.md). [How to cut a release](releasing.md) is for a maintainer, [How to verify a release](verifying-a-release.md) for anyone checking what they downloaded, and [How to install skift](install.md) for running one.
