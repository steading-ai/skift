# Security policy

## Reporting a vulnerability

Report a vulnerability privately, through GitHub: open the repository's **Security** tab and choose **Report a vulnerability**, or go straight to <https://github.com/steading-ai/skift/security/advisories/new>. Do not open a public issue or pull request for it.

Say what you found, which version or commit you found it in, and how to reproduce it. The report opens a private advisory that only you and the maintainers can read, and the fix and its disclosure are worked out there.

## Supported versions

Fixes go into the latest release. Skift is at 0.x, and earlier releases do not receive them.

## How a fix reaches you

A fix ships as a new release. A released `skift` embeds its dependencies, so a vulnerability in one of them is fixed the same way: Renovate proposes the patched version as soon as it exists, and a release follows once it has merged. [docs/ci.md](docs/ci.md#dependency-updates) describes that.

Every release is immutable, and each archive carries an attestation naming the workflow and commit that built it, and an SBOM listing what is inside it. [How to verify a release](docs/verifying-a-release.md) shows how to check a download before you run it.

## What skift trusts

Skift is a service you run yourself. It identifies each caller by the transport the call arrives on, and a caller holds no database credential. It is not a sandbox, an egress control or a supervisor. [Trust boundaries](docs/architecture.md#trust-boundaries) says where its responsibilities end.
