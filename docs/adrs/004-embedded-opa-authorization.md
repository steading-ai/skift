---
status: accepted
date: 2026-09-04
---

# Authorization by embedded OPA and Rego policy

## Context and Problem Statement

Dagger is a product that will eventually hold work descriptions in regulated environments, with agents and humans as principals across organizations and projects. Access control in incumbent tools is notoriously rigid. How should authorization be expressed so that it is flexible from the first deployment without being bolted on later?

## Decision Drivers

- Agents and humans share one API; what differs is what each principal may do
- Gates that only a human can complete, and ingest-class agents that may only file into a triage area, must be expressible without code changes
- Organizations will need bespoke rules; a policy language is cheaper to adopt now than to bolt on later, which is the whole reason for choosing it before a second organization exists
- Coding agents write and test Rego well
- No additional service to operate on a single-server deployment

## Considered Options

1. Embedded OPA via the Go Rego package, policy shipped as bundles
2. OPA as a sidecar service
3. Cedar
4. Hand-written role checks in Go

## Decision Outcome

Chosen option: **embedded OPA**, because it runs in-process as an interceptor after principal resolution, adds no service to operate, and gives organizations a standard policy language with its own test tooling. Input is principal, action, and resource. Default policy ships in the repository with an `opa test` suite.

Organization scoping is unconditional, static SQL that no policy can relax; OPA decides actions within that scope. Patches are authorized after alias resolution and cascade expansion, inside the writing transaction, because only then is the effective write set known. When row-level filtering beyond membership is required, OPA partial evaluation compiles policy into SQL predicates.

### Consequences

- **Good**: Human gates, ingest restrictions, and per-organization rules are policy, not features.
- **Good**: Policy is versioned and tested alongside code.
- **Bad**: A second language in the repository.
- **Bad**: Per-row evaluation does not scale; the SQL-scopes-then-OPA-decides split must be held to.
- **Neutral**: Field-level masking is deferred, so regulated data is out of scope until it lands. Response shaping is kept in one place so a mask set from policy can be applied later.

## Rejected Options

### OPA sidecar

- **Good**: Policy updates without redeploying dagger.
- **Bad**: Another process, another socket, more latency, more to operate.

### Cedar

- **Good**: Typed, formally verifiable, fast.
- **Bad**: Less team fluency, smaller ecosystem, Go support younger.

### Role checks in Go

- **Good**: Nothing to learn.
- **Bad**: Every new rule is a code change and a deploy; the permissions matrix incumbents suffer from.

## More Information

Follows [ADR-002](002-connect-rpc-is-the-only-door.md). Revisit the embedded-versus-sidecar choice if policy updates must be decoupled from deploys.
