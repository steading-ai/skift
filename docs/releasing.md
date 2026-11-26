# How to cut a release

This guide shows a maintainer how to release a version of skift: rehearse it, tag it, follow the run that publishes it, promote or yank it, and recover when a run fails. [ADR-011](adrs/011-releases-are-immutable-and-attested.md) has the reasons a release is built this way, [ADR-017](adrs/017-release-channels-and-packages.md) how it reaches users through the `edge` and `stable` channels, and [CI](ci.md) describes each workflow.

You need push access to the repository, the [GitHub CLI](https://cli.github.com) signed in as someone who administers it, and a checkout with the tools `mise install` provides.

## Set the repository up, once

A repository that has released before has all of this. Do it for a new one, or a fork that will publish its own releases.

1. Turn on [immutable releases](https://docs.github.com/en/code-security/concepts/supply-chain-security/immutable-releases), and check the answer is `true`:

   ```sh
   gh api -X PUT repos/steading-ai/skift/immutable-releases
   gh api repos/steading-ai/skift/immutable-releases --jq .enabled
   ```

2. Create an [environment](https://docs.github.com/en/actions/how-tos/deploy/configure-and-manage-deployments/manage-environments) named `release` whose deployment branches and tags are limited to the branch `main` and the tags `v*`. It holds no secret.

3. In the Claude Console, create a service account, register GitHub Actions as a federation issuer, and write a federation rule for this repository, as [Use WIF with GitHub Actions](https://platform.claude.com/docs/en/manage-claude/wif-providers/github-actions) describes. The rule must admit the token `release-notes.yml` presents: its audience is `https://api.anthropic.com`, its job runs in the `release` environment, and its event is the `push` of a tag or a `workflow_dispatch`. Write the rule against the subject the workflow prints in its log, not one composed from the repository's name: a subject can carry the owner's and the repository's numeric ids.

4. Give the environment the four identifiers the token exchange names, as variables. None is a secret:

   ```sh
   gh variable set ANTHROPIC_FEDERATION_RULE_ID --env release --body <federation rule id>
   gh variable set ANTHROPIC_ORGANIZATION_ID --env release --body <organization id>
   gh variable set ANTHROPIC_SERVICE_ACCOUNT_ID --env release --body <service account id>
   gh variable set ANTHROPIC_WORKSPACE_ID --env release --body <workspace id>
   ```

5. Once the repository has a release, prove the exchange with a dry run of the notes, which writes to the job summary and leaves the release as it is:

   ```sh
   gh workflow run release-notes.yml -f tag=<tag> -f dry_run=true
   ```

   A refused exchange answers HTTP 401 and nothing more. The reason is in the Claude Console's authentication history, and [workload identity federation](https://platform.claude.com/docs/en/manage-claude/workload-identity-federation) explains the terms.

## Choose the version

A person chooses the version; nothing derives it from the commit history. It is a plain version, `X.Y.Z`, with no pre-release part: the version says what changed, and the channel a build is in says how proven it is. It is the tag without its `v`, and it follows [semantic versioning](https://semver.org) over the public surfaces [ADR-014](adrs/014-what-the-version-covers.md) declares: the Connect API, the CLI, operator configuration, and the policy input. MCP tools, the database schema and Go packages are not among them.

While skift is at 0.x, take a new minor version for a change that can break a caller of a public surface and a patch version for anything else. A minor release may make such a change without a deprecation period; its release notes say what changed and what to do instead.

To see which image tags the release will set, given the versions already tagged: the version itself, and `edge` unless a later version is tagged. `stable`, `latest` and `X.Y` move only when the build is promoted.

```sh
git fetch --tags
make -s image-tags VERSION=<version>
```

## Rehearse it

Build what the release will upload, on your own machine:

```sh
make release VERSION=<version>
```

The archives, their SBOMs and `SHA256SUMS` land in `dist/release/`. It is the build the workflow runs.

There is no candidate tag. A release candidate is a release in `edge`: every release enters `edge` when it is published, and reaches `stable` when it is promoted.

## Tag the release

Tag the commit you mean to release with the version, and push the tag:

```sh
git tag -a v<version> -m v<version> <commit>
git push origin v<version>
```

Annotate the tag, with `-a`. The build refuses a tag that is not the version `make version` computes, and where two tags sit on one commit that is the newer annotated one.

Follow the run:

```sh
gh run list --workflow release.yml --limit 1
gh run watch <run id> --exit-status
```

## What the run does

The jobs of `release.yml`, in order:

- `build` checks that the tag is a plain version, the one the Makefile computes, runs `make release`, and attests each archive's provenance and SBOM. A tag with a pre-release part fails here, before anything is built.
- `image` has `release-image.yml` assemble the image from the linux binaries, push it by digest under no tag, and attest it.
- `compose` renders `docker-compose.yaml` with the image pinned to that digest, and adds it to `SHA256SUMS`.
- `draft` refuses an incomplete set of files, then puts every file on a draft release.
- `sign` signs the packslip manifest installers read.
- `publish` adds the manifest and publishes the draft. From here the release is immutable.
- `tag-image` gives the image the tags `make image-tags` prints, its version and `edge`, and refuses a version that is already tagged.
- `notes` has `release-notes.yml` replace the generated notes with written ones. Nothing waits on it.
- `verify` has `verify-release.yml` check the published release from outside and install it with mise on every platform.

## Promote or yank it

A published release is in `edge`. Promotion to `stable` is automatic, and the `pkgs.stead.ing` repository runs it, with the two manual controls: promote a security fix now rather than wait, or yank a bad build from both channels. [ADR-017's channels](adrs/017-release-channels-and-packages.md#2-two-channels-edge-and-stable) has the rule promotion follows and what a yank does. A yank stops a build spreading but does not take it off a machine that installed it, so follow it with a fixed release, which takes the next version.

## If the run fails

**Before `publish`**, nothing is published. The run leaves the tag, a draft release if `draft` got that far, and an image under no tag, which nothing refers to.

1. Delete the draft and its tag:

   ```sh
   gh release delete <tag> --cleanup-tag --yes
   ```

   If there is no draft, delete the tag alone:

   ```sh
   git push origin --delete <tag>
   git tag --delete <tag>
   ```

2. Fix the cause, and merge the fix.
3. Tag again. A version that was never published is still free.

**After `publish`**, the release exists and cannot change, so its version is spent: a published version is never reused, and a fix ships as the next version.

- If `notes` failed, the release keeps the notes GitHub generated. [Regenerate them](#regenerate-the-notes).
- If `tag-image` or `verify` failed for a reason outside the release, such as a runner or a registry that did not answer, run it again:

  ```sh
  gh run rerun <run id> --failed
  ```

## Regenerate the notes

`release-notes.yml` can be dispatched for any release. Read what it would write first, in the job summary of a dry run, then let it replace the release's title and notes:

```sh
gh workflow run release-notes.yml -f tag=<tag> -f dry_run=true
gh workflow run release-notes.yml -f tag=<tag>
```

A dispatched run uses the [communique.toml](../communique.toml) on `main`, whichever tag the notes are for. [communiqué](https://communique.jdx.dev) documents what that file configures.
