---
status: accepted
date: 2026-11-25
---

# What the version covers, and the rule before 1.0

## Context and Problem Statement

[Releasing](../releasing.md) says skift's version follows [semantic versioning](https://semver.org), and that while skift is at 0.x a change that can break a caller takes a new minor version. Semantic versioning starts from a declared public API (§1), and skift had none: nothing said whether a renamed CLI flag, a changed MCP tool, a new config key, or a migration counts as breaking a caller. Without that, "can break a caller" has no scope, and every release decision is argued afresh.

Which surfaces does the version make promises about, and what does it promise about them before 1.0?

## Decision Drivers

- Skift has three faces and several kinds of consumer: Connect clients and orchestrators, scripts calling the CLI, agents using MCP, operators running the service, and deployments writing their own policy
- [ADR-002](002-connect-rpc-is-the-only-door.md) makes the proto the domain model and the Connect API the only door
- A promise is only worth making where a consumer cannot adapt at run time
- Skift is pre-release and changes quickly; a deprecation process now would cost more than it protects

## Considered Options

1. Declare the public surfaces explicitly, with no deprecation periods before 1.0
2. Treat every interface, MCP included, as public
3. Declare nothing until 1.0

## Decision Outcome

Chosen option: **declare the public surfaces explicitly, with no deprecation periods before 1.0**, because a version number only informs a consumer if it says which changes it covers, and the surfaces differ in whether their consumers can adapt.

| Surface | Public | Why |
|---|---|---|
| Connect/protobuf API (`proto/skift/v1`) | Yes | The only door ([ADR-002](002-connect-rpc-is-the-only-door.md)). It covers the binary encoding, the JSON field and enum names ([ADR-008](008-short-enum-names-on-the-json-wire.md)), and behaviour: an unchanged schema whose meaning changes, such as an order or a refusal, is still a change. |
| CLI | Yes | Scripts call it. The contract is command names, flags, exit codes, the `SKIFT_*` environment variables and `-o json` output. The CLI's JSON is the wire's JSON, rendered through `internal/codec`, so it changes only when the API does. Text output is for people and may change in any release. |
| Operator configuration | Yes | A deployment depends on it across upgrades: server config keys, server environment variables, the systemd units and the compose file. |
| Policy input document | Yes | [ADR-004](004-embedded-opa-authorization.md) makes policy the extension point, so a deployment's own Rego depends on `input.principal`, `input.action` and the action names. |
| MCP tools | No | An agent reads tool schemas and descriptions every session and adapts to them, and agents run under a permission classifier rather than per-tool allow rules, so tool names carry no external dependency either. |
| Database schema | No | An implementation detail behind the API ([principle 2](../core-principles.md#2-one-door)). Upgrades migrate it; nothing outside skift may rely on its shape. |
| Go packages | No | Everything lives under `internal/`, which Go makes unimportable from outside the module. |

**Before 1.0**, a minor release may change public API without a deprecation period. Its release notes say what changed and what to do instead. A change that can break a caller of a public surface takes a new minor version; anything else takes a patch version.

### Consequences

- **Good**: "Can break a caller" has a definite scope, so choosing a version is a check against a list rather than a judgment call.
- **Good**: MCP stays free to improve its tools as fast as agents can use them.
- **Good**: No deprecation machinery to build or maintain while the interfaces are still settling.
- **Bad**: Before 1.0, a script or deployment can break on any minor upgrade; the release notes are the only warning.
- **Neutral**: Declaring the policy input public means its shape now needs the same care as the API's.

## Rejected Options

### Treat every interface as public

- **Good**: One rule for everything.
- **Bad**: MCP's consumers re-read it every session, so a compatibility promise there constrains skift without protecting anyone.

### Declare nothing until 1.0

- **Good**: No up-front decision.
- **Bad**: Semantic versioning has nothing to measure against, so a version number tells a consumer nothing, and the same arguments recur at every release.

## More Information

At 1.0, the public surfaces above gain deprecation periods: a public behaviour that is going away keeps working, with a warning, for a stated number of releases before it is removed, and the obligation to remove it lives in the code, so the release that should remove it cannot be built until it has. Until then, [Releasing](../releasing.md) applies the rule above when choosing a version.
