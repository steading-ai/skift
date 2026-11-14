# How to cut a release

This guide shows a maintainer how to release a version of skift: rehearse it, tag it, follow the run that publishes it, and recover when a run fails. [ADR-011](adrs/011-releases-are-immutable-and-attested.md) has the reasons a release is built this way, and [CI](ci.md) describes each workflow.

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

A person chooses the version; nothing derives it from the commit history. It is the tag without its `v`, and it follows [semantic versioning](https://semver.org). While skift is at 0.x, take a new minor version for a change that can break a caller and a patch version for anything else.

To see which image tags the version will take, given the versions already tagged:

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

Then tag a release candidate on the commit you mean to release, and push the tag:

```sh
git tag -a v<version>-rc.1 -m v<version>-rc.1 <commit>
git push origin v<version>-rc.1
```

Annotate the tag, with `-a`. The build refuses a tag that is not the version `make version` computes, and where two tags sit on one commit that is the newer annotated one.

A release candidate goes through the whole workflow and is published as a prerelease, immutable like any other release. Its image takes its exact tag and no moving one. Number candidates from 1 and never reuse a number.

## Tag the release

When a candidate's run is green, tag the same commit with the version and push it:

```sh
git tag -a v<version> -m v<version> <commit>
git push origin v<version>
```

Follow the run:

```sh
gh run list --workflow release.yml --limit 1
gh run watch <run id> --exit-status
```

## What the run does

The jobs of `release.yml`, in order:

- `build` checks that the tag is the version the Makefile computes, runs `make release`, and attests each archive's provenance and SBOM.
- `image` has `release-image.yml` assemble the image from the linux binaries, push it by digest under no tag, and attest it.
- `compose` renders `docker-compose.yaml` with the image pinned to that digest, and adds it to `SHA256SUMS`.
- `draft` refuses an incomplete set of files, then puts every file on a draft release.
- `sign` signs the packslip manifest installers read.
- `publish` adds the manifest and publishes the draft. From here the release is immutable.
- `tag-image` gives the image the tags `make image-tags` prints, and refuses a version that is already tagged.
- `notes` has `release-notes.yml` replace the generated notes with written ones. Nothing waits on it.
- `verify` has `verify-release.yml` check the published release from outside and install it with mise on every platform.

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
3. Tag again. A release candidate takes the next number. A version that was never published is still free.

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
