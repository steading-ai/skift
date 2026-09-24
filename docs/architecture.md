# Architecture

How the pieces fit. The decisions behind them are in [docs/adrs/](adrs/); this describes the arrangement.

## One binary, three faces

```
skift              CLI. Connect client, JSON output
skift mcp          stdio MCP server. The same client, wrapped for a harness
skift serve        The service. Connect handlers over PostgreSQL
```

The CLI and the MCP server are the same client with different framing, so an agent reaching skift through Claude Code and an operator typing at a terminal exercise identical code. Which transport that client uses is configuration, not a code path.

## Request path

```
agent / human / orchestrator
        │
        ├── skift, skift mcp                 (client)
        │
        ▼
   Connect  ── unix socket │ tsnet │ HTTPS
        │
        ▼
  ┌───────────────────────────────────────────┐
  │ interceptors                              │
  │   1. resolve principal from the transport │
  │   2. coarse method-level authorization    │
  │   3. rate limit  (deferred)               │
  └───────────────────────────────────────────┘
        │
        ▼
     handlers        wire types only
        │
        ▼
      domain         graph rules: cycles, readiness, leases, aliases
        │              policy on the resolved patch, inside its transaction
        │
        ▼
      store          sqlc over pgx
        │
        ▼
    PostgreSQL       nodes, edges, leases, events, usage, comments
```

Authorization happens twice, and the split matters. The interceptor knows who is calling and whether they may call this method at all. It cannot know which resources a patch touches, because aliases are unresolved and cascades unexpanded until the domain layer applies it. So the effective write set is authorized inside the writing transaction, against the snapshot it writes to, and one denial rolls back the patch.

Handlers never see the transport, and never reach the store directly. The domain layer owns every rule that makes the graph a graph, which is what keeps a second storage backend possible and what makes the rules testable without a server.

## Identity by transport

One `Principal` type, several resolvers, added in this order:

| Transport | Resolver | When |
|---|---|---|
| Unix socket | `SO_PEERCRED`, uid to principal | Same host. Agents in grove, service in wellspring |
| tsnet | Tailscale `WhoIs` | Agents on other machines, including a laptop |
| HTTPS | Bearer token or OIDC | Other organizations, later |

An identity is a row, not a principal, so one principal may hold several. Adding a transport inserts rows rather than migrating. See the Principal section of the [domain model](domain-model.md).

## Deployment

Skift is a wellspring service in [homestead](https://github.com/ivy/homestead) terms: it shares the platform PostgreSQL, and agents in application zones reach it as clients. That places it where Artifactory sat in the incident [ADR-006](adrs/006-provenance-not-authority.md) describes, which is why tenancy scoping is load-bearing rather than convenient.

```
wellspring                         grove
  skift.service   ◄── unix socket ── agents, orchestrator
  postgresql
  skift-reap.timer                laptop
                   ◄── tsnet ────── skift, skift mcp
```

| Unit | Does |
|---|---|
| `skift.service` | The Connect service. Logs to the journal, notifies readiness with sd_notify |
| `skift-reap.timer` | Expires stale leases. The only background work in v1 |

## Consumers

Skift stores and serves. Everything reactive is a separate program reading the [change stream](domain-model.md#change-stream):

| Consumer | Reads | Does |
|---|---|---|
| Orchestrator | `ClaimNext` with a wait | Dispatches agents, holds leases, reports usage |
| Notifier | Gate nodes on the stream | Tells a human over Matrix that work needs them |
| Retrieval | Content events | Indexes for semantic search and synthesis. Successor to [kb](https://github.com/ivy/kb) |
| Moderation | Content events | Inspects what enters the backlog |

None of them are skift, and skift works without any of them.

## Trust boundaries

- **Between agent and skift.** The agent holds no database credential. Every write carries a transport-resolved principal it cannot forge.
- **Between projects.** Tenancy columns on every row, every query scoped, cross-project edges permitted but policy-gated.
- **Between skift and the platform.** Skift is not a sandbox, not an egress control, and not a supervisor. Compute isolation and network policy are homestead's.

## What v1 is

The walking skeleton, in one sentence: one organization, one project, over a unix socket, an agent can create and link nodes in a patch, ask what is ready, claim it, and complete it with a result, and every step of that is an event.

It includes the correctness work that is cheap now and expensive later: lease fencing, the advisory lock on edge-mutating patches, cycle detection over the combined wait relation, in-transaction patch authorization, and fixed structural ceilings.

Deferred until that runs daily: artifacts, search, comments, the change stream, tsnet, the MCP server, delegation, rate limits, budgets. Milestones beyond it are in the [roadmap](roadmap.md).
