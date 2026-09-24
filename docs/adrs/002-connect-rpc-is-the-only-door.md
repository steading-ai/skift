---
status: accepted
date: 2026-09-04
---

# Connect-RPC is the only door, with identity resolved by transport

## Context and Problem Statement

Agents, humans, orchestrators, a CLI, an MCP server, and eventually a web UI all need to read and mutate the graph. Agents run on the same host in a different unix account, on other machines over a tailnet, and eventually in other organizations over the internet. What is the API boundary, and how does a caller's identity reach it?

## Decision Drivers

- The API contract must be the domain model so that storage and transport can change underneath it
- Wide SDK availability across languages for a product
- Credential isolation: agents must not hold database credentials
- One host today with unix sockets and SELinux, a tailnet next, the internet later
- Streaming for the ready set so orchestrators need not poll

## Considered Options

1. Connect-RPC over unix socket, tsnet, and HTTPS, with a principal-resolving interceptor
2. gRPC only
3. REST with OpenAPI
4. Direct SQL access with per-principal database roles

## Decision Outcome

Chosen option: **Connect-RPC with transport-resolved identity**, because Connect serves the same protobuf contract over HTTP/1.1 and HTTP/2 to browsers, curl, and every gRPC-capable language, and because putting identity resolution in an interceptor keeps handlers unaware of whether a request arrived over a socket, the tailnet, or the internet.

One `Principal` type. Three resolvers, added in this order:

1. `SO_PEERCRED` on a unix socket, mapping a unix uid to a principal
2. Tailscale `WhoIs` via `tsnet`
3. Bearer token or OIDC over HTTPS

Server streaming, backed by PostgreSQL `LISTEN`/`NOTIFY`, serves change events. Orchestrators do not stream the ready set: they call `ClaimNext`, which waits and returns work already claimed under a fencing token. Readiness is derived and emits no event of its own, so a ready stream would mean storing derived state.

### Consequences

- **Good**: Agents never hold database credentials. The credential isolation principle of the orchestrator is preserved.
- **Good**: The MCP server and CLI are one Go client with a transport configuration. The same binary serves as `skift`, `skift mcp`, and `skift serve`.
- **Good**: Proto types are versioned as `skift.v1` from the start.
- **Bad**: Two models, wire and storage, with a mapping layer between them. This is the price of swappability and is accepted deliberately.
- **Bad**: Proto contract design is front-loaded work.
- **Neutral**: `tsnet` is deferred until agents run on a second machine. Unix sockets suffice on one host.

## Rejected Options

### gRPC only

- **Good**: Mature.
- **Bad**: HTTP/2 only, no browser without a proxy, harder to curl.

### REST with OpenAPI

- **Good**: Universal.
- **Bad**: Streaming is awkward, contract drift is common, and batch atomic mutation does not map naturally.

### Direct SQL with database roles

- **Good**: No API layer at all.
- **Bad**: Every client holds a database credential, the schema becomes the contract, and swapping storage becomes impossible.

## More Information

Depends on [ADR-004](004-embedded-opa-authorization.md) for what happens after a principal is resolved.
