---
status: accepted
date: 2026-11-26
---

# Every release is a plain version, promoted from edge to stable, and signed into pkgs.stead.ing

## Context and Problem Statement

[ADR-011](011-releases-are-immutable-and-attested.md) defines a release: a tag, built by goreleaser, published by the workflow, immutable and attested. It did not anticipate a package repository. It marks a tag with a pre-release part, such as `v0.2.0-rc.1`, as a GitHub prerelease. It leaves `make package` on the Makefile's own `go build`, so the rpm holds a binary no release attests. It says nothing else signs, and rejects a GPG-signed `SHA256SUMS` because it needs a long-lived key in CI. It moves the image's `X.Y`, `X` and `latest` tags when a stable release is published. And it puts package repositories out of scope.

Skift is meant to ship several times a week, if not daily, and, given the security landscape with agents, to upgrade unattended, automatically and uneventfully, with no toil on its users. The maintainer wants every release on a real server as soon as it is cut, to shorten the feedback loop, and everyone else to get a build only once it has proved itself. An rpm installed by hand from a checkout does neither, and a pre-release tag per candidate means the build people finally run is a rebuild, not the build that was tried.

How do releases reach users' machines, how is a build trusted before most of them get it, and what signs the packages dnf installs?

## Decision Drivers

- The build a cautious user runs is the exact build that was tried first, not a rebuild of the same commit under another version
- Every release reaches the maintainer's own server when it is published; everyone else gets it once it has soaked, without anyone doing anything
- Releases ship daily, so the cautious path must keep moving at that pace, and a security fix must be able to skip the wait
- A consumer can check a package back to the workflow and commit that built it, as ADR-011 lets them check an archive
- dnf verifies OpenPGP signatures on packages and repository metadata, and nothing else; a GitHub attestation is not one
- A release is immutable, and a repository's metadata changes with every release, so the repository cannot live in a release
- The repository behaves like a distribution's own: installing it is explicit, it is verified, and its packages touch only their own files
- One answer serves every Steading component, so a second component joins without designing any of this again

## Considered Options

1. Plain versions, promoted from `edge` to `stable` in a Steading package repository that signs what it has verified
2. Pre-release versions, a suffixed tag for each candidate
3. Fedora COPR

## Decision Outcome

Chosen option: **plain versions, promoted from `edge` to `stable` in a Steading package repository that signs what it has verified**, because it is the only option in which the build that soaked is byte for byte the build that `stable` installs, and in which the chain from a package back to the attested build that produced it is never broken.

The decision has six parts. They hold for every Steading component from here on, starting with skift.

### 1. Plain versions

Every release is a plain [SemVer](https://semver.org) version, `X.Y.Z`, with no pre-release suffix, and is never a GitHub prerelease. The version says what changed in the public API, as [ADR-014](014-what-the-version-covers.md) defines it; the channel says how proven the build is. A release candidate is a release in `edge`, not a suffixed tag.

This replaces the tilde scheme, in which the rpm of `v0.4.0-rc.2` carried the version `0.4.0~rc.2` so that rpm would sort it before `0.4.0`. Under that scheme a candidate that proves good still has to be rebuilt as `0.4.0` before anyone outside can run it.

### 2. Two channels, `edge` and `stable`

`edge` gets every release when it is published. `stable` gets the same signed rpm when it is promoted. Promotion changes only repository metadata: the rpm is neither rebuilt nor signed again. The model follows [Fedora CoreOS's update streams](https://docs.fedoraproject.org/en-US/fedora-coreos/update-streams/) and Claude Code's release channels.

**Promotion is automatic.** `stable` moves to the newest `edge` build that has been in `edge` for at least 48 hours and has not been yanked. Choosing the newest build that has soaked, instead of restarting the clock on every release, keeps `stable` moving when releases ship daily: a build released every day still reaches `stable` two days later.

**Two manual controls.** _Promote now_ moves a build to `stable` without waiting, for a security fix. _Yank_ removes a build from both channels, which stops it spreading. dnf never downgrades a machine that already installed a yanked build, so the fix for those machines is a new, higher version.

Rejected: manual promotion. It is toil on every release, and `stable` falls behind whenever nobody does it.

Rejected: promotion on a fixed weekly schedule. It holds a security fix back until the next slot.

Rejected: promotion gated on health reports from canaries. With one canary it waits on a signal that does not yet exist; it becomes worth revisiting when there is more than one.

### 3. The image follows the channels

The image gets `edge` and `stable` tags that move with the channels. A release moves `edge`. Promotion moves `stable`, and with it the moving tags ADR-011 gave a stable release: `latest`, `X.Y`, and `X` from 1.0 on, each still only forward. `X.Y.Z` never moves. A digest pin remains the immutable reference.

### 4. rpms are release assets

A component's release carries rpms for x86_64 and aarch64, built from the same linux binaries its archives hold, attested as the archives are, and covered by `SHA256SUMS`. The rpm is not built separately, so the binary in a package is a binary the release attests.

### 5. One repository for the suite: `pkgs.stead.ing`

`pkgs.stead.ing` serves every Steading component's packages, starting with Fedora. One repository per channel serves every Fedora version, with no split by `$releasever`, because the binary is static. Each channel keeps every release forever: dnf downloads only metadata, so a long history costs storage and not correctness, and every version stays available to downgrade to or to audit. A skift release is about 60 MB of rpms across both architectures, so daily releases add about 22 GB a year.

A GitHub repository named `pkgs.stead.ing`, after the name it serves, holds the list of components, the publishing and promotion workflows, the signing environment, and the site. It owns channel state for packages and images both, so one record of which build is in which channel drives the repository metadata and the image's channel tags. A component joins by adding a line to the list, not by changing its own release.

The repository is installed with a `steading-release` rpm, or with a `.repo` file and a fingerprint published on the site. `gpgcheck` and `repo_gpgcheck` are both on. A package never edits repository configuration, never enables a service, and touches nothing outside its own files.

### 6. Signing

One Steading OpenPGP key, with no expiry, signs every rpm and every repository's metadata. The private key is a secret in a protected environment of the `pkgs.stead.ing` repository, usable only by its publishing job on `main`, with an offline copy. The workflow signs only rpms whose attestation it has verified against the component's release workflow, so the signature extends the chain ADR-011 built instead of replacing it.

This is a long-lived key in CI, which ADR-011 rejected. It is accepted here because dnf checks only OpenPGP signatures, and it is confined: component releases still hold no key, so ADR-011's driver holds for them, and the key exists in one environment of one repository.

The key is rotated only if it is compromised. The `steading-release` rpm carries the public key, so a rotation is an update to that package, and users do nothing.

Rejected: a key that expires, renewed every year. Forgetting to renew it breaks every user's upgrades at once, and a yearly renewal is a key ceremony nobody wants to hold.

Rejected: a key held in a cloud key service. No proven tooling signs rpms and repository metadata with one.

### Out of scope

deb, Arch and Homebrew packages; CentOS and RHEL, though the static binary likely makes the Fedora repository work there with little effort; an install script such as `curl https://get.stead.ing | sh`; and self-updating binaries. Where `pkgs.stead.ing` is hosted is the `pkgs.stead.ing` repository's to decide.

### Consequences

- **Good**: A `stable` user runs the exact bytes the maintainer's own server ran for at least two days, and a security fix can reach them at once.
- **Good**: Every package traces back to the attested release build that produced it, and its signature is applied only after that trace is checked.
- **Good**: Installing and upgrading skift on Fedora is `dnf`, verified end to end, with nothing for the user to do after the first install.
- **Good**: A second component reaches users in both channels by joining a list, not by building its own repository, key or promotion.
- **Bad**: A long-lived signing key now exists, and its compromise means a key rotation every user receives through a package update.
- **Bad**: Every release is GitHub's latest the moment it is published, and an installer that takes the newest release, such as mise, follows `edge` rather than `stable`.
- **Bad**: A yanked build stays on every machine that installed it until a higher version ships.
- **Neutral**: A version number no longer says how proven a build is; which channel holds it does.
- **Neutral**: The image's `main` and commit tags, pushed on every commit to `main`, are builds, not releases, and belong to neither channel.

## Rejected Options

### Pre-release versions, a suffixed tag for each candidate

Tag `vX.Y.Z-rc.N` for each candidate, publish it as a GitHub prerelease, and package it as `X.Y.Z~rc.N` so that rpm sorts it before the final version.

- **Good**: The version itself says the build is a candidate, and installers that skip prereleases skip it.
- **Bad**: A candidate that proves good must be rebuilt and re-released under the final version, so what users run is not the build that was tried. At daily releases every build would carry a suffix, and the suffix would say nothing a channel does not.

### Fedora COPR

Fedora's community build service builds packages from source, signs them with a key it holds, and serves them as a repository.

- **Good**: Hosting, signing and repository metadata come for free.
- **Bad**: It rebuilds from source, so the package holds a binary no release attests, and the chain back to the attested build is broken.

## More Information

This record amends ADR-011 where they meet: its pre-release tags, its `make package`, its asset table, its signing, its image tags, and its exclusion of package repositories. [ADR-015](015-upgrades-and-migrations.md) sets what an upgrade must survive. [How to cut a release](../releasing.md) is the maintainer's guide. Revisit health-gated promotion when there is more than one canary.
