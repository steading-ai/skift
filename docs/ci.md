# CI

The only workflow this repository runs is [publish.yml](../.github/workflows/publish.yml):
on every push to `main`, it cross-compiles `skift` for linux/amd64 and
linux/arm64 (the same `dist/skift-linux-%` build the Makefile uses for the
rpm) and pushes a multi-arch image to GHCR, built from
[packaging/Containerfile](../packaging/Containerfile). It does not run on
pull requests — it publishes, it does not check anything.

## The image

- **Name:** `ghcr.io/steading-ai/skift`
- **Tags:** the full commit SHA, one per push — immutable, pinnable — and
  `main`, which moves to whatever the latest push produced.
- **Contents:** `skift` at `/usr/local/bin/skift`, on a `scratch` base. Nothing
  else — no shell, no entrypoint. `podman run --rm
  ghcr.io/steading-ai/skift:<sha> skift version` runs `skift` itself as the
  container's command.
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
