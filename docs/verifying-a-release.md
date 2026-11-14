# How to verify a release

This guide shows how to check that what you downloaded from a skift release is what this repository's release workflow built, and what is inside it: an archive, the release as a whole, the image, and an SBOM.

If you install with mise's `packslip:` backend, as [How to install skift](install.md) does, mise checks the archive for you: it verifies the release's signed manifest and pins who signed it.

You need the [GitHub CLI](https://cli.github.com), signed in, and `jq` to read an SBOM. In the commands, `<version>` is the release's version without its `v`, and `<os>_<arch>` is `linux_amd64`, `linux_arm64` or `darwin_arm64`.

## Check an archive

```sh
gh attestation verify skift_<version>_<os>_<arch>.tar.gz --repo steading-ai/skift --signer-workflow steading-ai/skift/.github/workflows/release.yml --source-ref refs/tags/v<version>
```

It succeeds only when `release.yml` in this repository built that file from that tag. Leave `--source-ref` out to accept the workflow's build of any tag. GitHub documents the command under [artifact attestations](https://docs.github.com/en/actions/how-tos/secure-your-work/use-artifact-attestations).

## Check the release

To check GitHub's own attestation of the release, which records its tag, its commit and its assets as they were published:

```sh
gh release verify v<version> --repo steading-ai/skift
```

To check that a file you hold is one of its assets:

```sh
gh release verify-asset v<version> <file> --repo steading-ai/skift
```

To check every file you downloaded against the release's checksums:

```sh
sha256sum --check --ignore-missing SHA256SUMS
```

`SHA256SUMS` covers every asset but `packslip.sigstore.json`, the manifest mise reads.

## Check the image

A release that carries an image names it in its `docker-compose.yaml`, as `ghcr.io/steading-ai/skift:<version>@sha256:<digest>`. Check the compose file is the release's, read the digest from it, and verify the image by that digest:

```sh
gh release verify-asset v<version> docker-compose.yaml --repo steading-ai/skift
grep 'image: ghcr.io/steading-ai/skift' docker-compose.yaml
gh attestation verify oci://ghcr.io/steading-ai/skift@sha256:<digest> --repo steading-ai/skift --signer-workflow steading-ai/skift/.github/workflows/release-image.yml --source-ref refs/tags/v<version>
```

The image's signer is `release-image.yml`, the workflow that pushes it, not `release.yml`. Run the image by the same digest, as the compose file does, and what runs is what you verified whatever its tags do later.

## Read an SBOM

Each archive has a CycloneDX SBOM beside it, `skift_<version>_<os>_<arch>.cdx.json`. To list every module in the binary with its version:

```sh
jq -r '.components[] | "\(.name) \(.version)"' skift_<version>_<os>_<arch>.cdx.json
```

To find one, such as the module a vulnerability report names:

```sh
jq -r '.components[] | select(.name == "golang.org/x/crypto") | .version' skift_<version>_<os>_<arch>.cdx.json
```

To check that the release workflow attested an SBOM for the archive, and to print the attested SBOM itself:

```sh
gh attestation verify skift_<version>_<os>_<arch>.tar.gz --repo steading-ai/skift --signer-workflow steading-ai/skift/.github/workflows/release.yml --predicate-type https://cyclonedx.org/bom --format json --jq '.[0].verificationResult.statement.predicate'
```

The image's SBOM is attested the same way: give the command the `oci://` reference and `release-image.yml` as the signer.

[ADR-011](adrs/011-releases-are-immutable-and-attested.md) records what a release contains and why it is signed this way.
