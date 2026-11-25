# Architecture

How the pieces fit. The decisions behind them are in [docs/adrs/](adrs/); this describes the arrangement.

## One binary, three faces

```
skift              CLI. Connect client, JSON output
skift mcp          stdio MCP server. The same client, wrapped for a harness
skift serve        The service. Connect handlers over PostgreSQL; optional native HTTP MCP
```

The CLI and stdio MCP server are the same client with different framing. Native HTTP MCP is a framing adapter over those same generated Connect services, dispatched in-process without a loopback connection. The shared tool adapter owns argument conversion and result rendering, not graph rules. [ADR-011](adrs/012-native-streamable-http-mcp.md) is the native HTTP contract; its implementation is separate from this architecture decision.

## Request path

```
agent / human / orchestrator
        │
        ├── skift, skift mcp                 (client)
        │       │
        │       ▼
        │   Connect ── unix socket │ tsnet :80
        │       │
        └── native HTTP MCP ── tsnet TLS :443 /mcp (opt-in)
                │ original transport context, shared tools
                ▼
            in-process generated Connect client/handler dispatch
                │
                ▼
          same Connect services/interceptors
        │
        ▼
  ┌───────────────────────────────────────────┐
  │ interceptors                              │
  │   1. resolve principal from the transport │
  │   registered procedure dispatch          │
  │   rate limit  (deferred)                 │
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
      store          every SQL statement: sqlc over pgx
        │
        ▼
    PostgreSQL       nodes, edges, leases, events, usage, comments
```

Authentication and resource authorization are separate. The principal interceptor resolves who is calling. All mapped principals may invoke the registered Connect procedures; there is no role-based method authorization interceptor in the current implementation. Earlier architecture/ADR wording that described such an interceptor was ahead of the code. Do not confuse the generated procedure router or identity lookup with a method policy gate. A future role-specific method policy needs its own explicit decision.

The domain owns action/resource authorization. An interceptor cannot know which resources a patch touches: aliases are unresolved and cascades unexpanded until domain applies it. The effective write set is authorized inside the writing transaction, against the snapshot it writes to, and one denial rolls back the patch. Tenant scoping, policy decisions, and lease holder/fencing checks remain in that same path for native MCP and Connect.

Handlers never see the transport, and never reach the store directly. The domain layer owns every rule that makes the graph a graph, as decisions: which question to ask the store, and what the answer means. The store is the only package that reaches PostgreSQL. Every SQL statement, connection, and `LISTEN` lives there, and the domain layer cannot execute SQL at all. See [ADR-013](adrs/013-postgresql-only-through-the-store.md).

## Identity by transport

One `Principal` type, several resolvers, added in this order:

| Transport | Resolver | When |
|---|---|---|
| Unix socket | `SO_PEERCRED`, uid to principal | Same host. Agents in grove, service in wellspring |
| tsnet | Tailscale `WhoIs` | Agents on other machines, including a laptop |
| HTTPS | Bearer token or OIDC | Other organizations, later |

An identity is a row, not a principal, so one principal may hold several. Adding a transport inserts rows rather than migrating. See the Principal section of the [domain model](domain-model.md).

## Native HTTP MCP contract

The optional service endpoint is `https://<canonical-tailnet-name>/mcp`, using a second, tailnet-only TLS listener (default 443). Existing port-80 Connect clients and unix-socket clients are unchanged; neither serves MCP. `--mcp-http` / `SKIFT_MCP_HTTP` opts in, with `--mcp-http-port` / `SKIFT_MCP_HTTP_PORT` selecting the TLS port. It requires the existing Tailscale node, MagicDNS and HTTPS certificate support; `tsnet.ListenTLS` uses that node's local certificate service. No public listener, Funnel, proxy-trusted identity, plaintext fallback, or public OAuth is added.

The official Go SDK is pinned to `github.com/modelcontextprotocol/go-sdk v1.8.0`, targeting **only MCP 2026-07-28 on native HTTP**. It provides `server/discover`, per-request `_meta`, mirrored-header validation and request-scoped SSE. No initialize handshake, session ID, GET stream, legacy HTTP+SSE, change subscriptions or resumable streams are required or offered. Older HTTP revisions are explicitly rejected and tested; existing stdio revisions stay unchanged. Clients accept both JSON and SSE; normal success uses SSE and protocol validation errors may use JSON.

Configure SDK stateless mode and request cancellation propagation explicitly; Origin checks are not enabled by the SDK by default. Outer middleware validates Host against the canonical tailnet name and accepts only an absent Origin or the exact canonical HTTPS Origin. Every request, including discovery and tool listing, uses the existing transport resolver/identity lookup, shared with `NewPrincipal`. Tool calls use generated Connect clients backed by a direct in-process HTTP handler dispatcher into the Connect-only mux. The original accepted peer context, cancellation/deadline, and drain signal survive dispatch; the same principal interceptor and domain policy run. Never use a localhost socket's service credentials or client-selected identity headers. There is no second domain API or policy store.

v1.8.0 has a verified `Mcp-Name` Base64-decoding gap. ADR-011 permits only a small, cloned-header normalization for registered safe ASCII tool names; the SDK still validates name/body agreement. This is not a claim of arbitrary Unicode-name conformance. Its tests and exact limitations are part of the implementation contract, not grounds to write a custom protocol engine.

HTTP lease lifetime is separate from connection lifetime. A claim survives its response until explicit completion/release or expiry. HTTP starts no per-client renewer and releases no client's leases at service shutdown. Clients use the shared `heartbeat`, `release`, and `complete` tools. Stdio retains automatic renewal and shutdown release. Disconnect cancels work but cannot undo an already committed claim; retry with the same domain request ID or inspect the graph. The TLS listener joins the existing readiness and bounded drain lifecycle; waiting claims receive the same drain signal.

See ADR-011 for the HTTP response matrix, scoped SDK workaround, security boundaries, exact APIs, verification evidence, and required Ginkgo scenarios.

## Deployment

Skift is a wellspring service in homestead terms: it shares the platform PostgreSQL, and agents in application zones reach it as clients. That places it where Artifactory sat in the incident [ADR-006](adrs/006-provenance-not-authority.md) describes, which is why tenancy scoping is load-bearing rather than convenient.

```
wellspring                         grove
  skift.service    ◄── unix socket ── agents, orchestrator
  postgresql
  skift-reap.timer                 laptop
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
