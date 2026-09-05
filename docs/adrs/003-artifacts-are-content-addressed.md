---
status: accepted
date: 2026-09-04
---

# Artifacts are content-addressed: managed blobs plus external references

## Context and Problem Statement

Work produces outputs beyond text: pull requests, patch sets, zips of design assets, bundles of markdown. Downstream agents consume them as inputs. Every project management system supports attachments, but dagger runs in a distributed setting where agents on different machines produce and consume. How are artifacts stored and delivered without turning dagger into a distributed filesystem, and without pushing storage onto every orchestrator?

## Decision Drivers

- Downstream agents must be able to discover and verify upstream outputs
- Orchestrators should not each solve storage; that creates sprawl and configuration
- Large data must not flow through the project management system
- Existing platform services: git hosting, an OCI registry, one PostgreSQL

## Considered Options

1. References only: dagger stores URIs and digests, never bytes
2. Managed blob store only: everything uploaded to dagger
3. Both: a managed, size-capped, content-addressed blob store behind a storage interface, plus first-class external references

## Decision Outcome

Chosen option: **both**, because attachments are commonplace and centralizing them prevents sprawl, while a size cap and content addressing keep dagger from becoming a data transport. Coding agents already produce git refs and design agents can push to an OCI registry; those stay where they are and dagger records a digest.

The artifact shape follows A2A: name, description, parts, metadata. A part is inline text under a cap, a managed blob, or a reference. Every blob and reference carries a digest.

- **Managed blobs**: immutable, content-addressed, size-capped per organization. Storage backend is an interface. Filesystem first, object storage later.
- **References**: `git` for branches, commits, and pull requests. `oci` for arbitrary blobs in a registry, pushed with ORAS. `https` as a fallback. The scheme is the contract and the client resolves it.

### Consequences

- **Good**: Downstream agents verify what they pulled by digest regardless of transport.
- **Good**: Orchestrators need no storage configuration. Small outputs go to dagger, large ones to the registry.
- **Good**: OCI artifacts are deduplicated, signable with cosign, and already operated in the platform zone.
- **Bad**: Two artifact paths to document and test.
- **Bad**: A reference can point somewhere a given client cannot reach. Mitigated by scheme conventions, not by dagger proxying.
- **Neutral**: Attachments and comments are a surface a swarm can abuse as a message board. Size caps and policy are the controls, and this is tracked as an open discussion.

## Rejected Options

### References only

- **Good**: Simplest service.
- **Bad**: Every orchestrator must solve storage. Sprawl.

### Managed blobs only

- **Good**: One path.
- **Bad**: Dagger becomes the transport for large data and the git and OCI content that already has a home gets copied.

## More Information

Revisit size caps and backend choice when the first object storage deployment happens.
