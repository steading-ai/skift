# CI

Two workflows run here.

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

[publish.yml](../.github/workflows/publish.yml) runs on every push to
`main`: it cross-compiles `skift` for linux/amd64 and linux/arm64 (the same
`dist/skift-linux-%` build the Makefile uses for the rpm) and pushes a
multi-arch image to GHCR, built from
[packaging/Containerfile](../packaging/Containerfile). It does not run on
pull requests — it publishes, it does not check anything.

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

GHCR is the one place on GitHub where another repository's workflow can
pull with nothing but its own `GITHUB_TOKEN`: a package can grant a
repository read access to Actions. This is why skift publishes an image
here rather than a release asset — a private repository's release assets
grant no such thing.

That grant is a person's step, not this workflow's, and it happens once
per consuming repository: on the package's page
(`https://github.com/orgs/steading-ai/packages/container/skift`), under
**Package settings → Manage Actions access**, add the consuming
repository with the **Read** role. Once granted, that repository's
workflows can `docker login ghcr.io -u <actor> -p $GITHUB_TOKEN` and pull
the image with no further credential.
