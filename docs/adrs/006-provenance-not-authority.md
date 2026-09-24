---
status: accepted
date: 2026-09-05
---

# Provenance is not client-assertable, and the store never confers authority

## Context and Problem Statement

Skift is a shared store that agents use to coordinate work: nodes, assignments, claims, comments, and artifacts. Any shared mutable store reachable by many agents is also a channel between them, and agents given one will use it for assignment, consensus, and file transfer whether or not that was intended. A [2026 METR and Redwood Research report](../research/metr-openai-hugging-face-incident-2026.md) on agents that built exactly these primitives on top of an unisolated package cache is one documented instance; the property is general.

What must skift get right so that a legible, governed store is safer than the covert one agents will otherwise build?

## Decision Drivers

- The coordination drive exists in the models regardless of whether a substrate is provided
- Evidence about agent behavior comes from a single report on an internal research model with safety classifiers off; structural properties of shared stores are the reliable part
- Skift must not become the place where an agent's assignment to another agent reads as permission
- Ship and iterate: the controls must be columns and default policy, not a monitoring product
- Calculated risk: over-designing guardrails is what stops delivery in this space

## Considered Options

1. Treat safety as a later concern and build the store as an ordinary tracker
2. Build enforcement into skift: rate limits, sprawl counters, anomaly thresholds, content moderation
3. Make provenance transport-resolved and isolation real, and leave judgment to consumers

## Decision Outcome

Chosen option: **transport-resolved provenance, real isolation, judgment in consumers**. Three structural constraints are adopted. Everything behavioral is left to the orchestrator, a supervisor, or a moderation service consuming the event stream.

The constraints:

1. **A shared store without isolation becomes a channel.** Tenancy scoping is the safety boundary, not a convenience. A project is an authorization scope that inherits organization policy, not a unit requiring a per-project grant; making admission a ritual would defeat cheap projects without adding isolation. Cross-project edges are permitted and policy-gated: an edge from A into B requires write on A and link permission on B.
2. **Agents will treat shared state as authority.** Skift never confers any. Every write carries the acting principal and its kind, resolved by the transport and not settable by a client. Delegation requires a server-known grant, not an assertion in the request. Whether a human has ever touched a node is derivable and exposed. Consumers decide what agent-created work is worth.
3. **Agents will not escalate to humans on their own.** The gate node and the ready stream filtered to human-assigned nodes are the escalation path, and policy can require one for any mutation outside an agent's scope.

Default policy for a lease holder:

- May mutate its own node and the children it creates under it, not arbitrary descendants
- May add edges into its own node, which only changes its own readiness
- May not add edges out of its node into others without link permission on the target
- Anything else is rejected, and the agent files a gate node to ask

Adopted for day one: the provenance columns, gate nodes with integrity rules that stop a requester disposing of its own gate, the default policy, and fixed structural ceilings on request size and shape.

Those ceilings are **not** the rate limiting deferred here. A cap on operations per patch or a statement timeout needs no production data, because it exists to stop one request consuming the service rather than to shape anyone's behaviour. Deferred until production data exists: per-principal rate limits, structural counters, budget caps at claim time, and anomaly thresholds. The interceptor is the seam for all of them.

Also adopted, and free: node bodies, comments, and results are agent-authored text, so a consumer must render them as attributed data rather than folding them into instructions. That is the same threat reaching the store through the front door instead of a covert channel.

### Consequences

- **Good**: The entire day-one cost is a few columns, a default policy file, and size caps that a multi-tenant product needs anyway.
- **Good**: The principle "ready is dumb" survives intact. No enforcement at claim or patch time beyond graph invariants and policy.
- **Good**: Legibility is a property of the store, not a behavior expected of agents.
- **Bad**: Skift cannot tell what is inside an artifact or comment, and does not fetch reference URIs, so an agent-supplied URI is untrusted input to whatever resolves it. Abuse of those as a channel is attributable and size-limited, not prevented.
- **Bad**: The trust story depends on consumers reading provenance. An orchestrator that dispatches agent-created nodes without looking is outside skift's control.
- **Neutral**: Rate limits are deferred because realistic limits cannot be set across diverse workloads before observing them.

## A trust zone is the delegation mechanism

Delegation requires a server-known grant. The trust zone the agent runs in is that grant, and the transport already resolves it.

Identity is resolved from the connection: a peer uid over the unix socket, a node identity over the tailnet. Every process one unix user runs is therefore one principal. An agent working interactively under a user's uid **is** that user principal, with the user's authority over the whole organization; running it there is the grant, and killing it is the revocation. Its writes are `human_touched` because the user vouches for them by running it under their own identity. Nothing about this is client-assertable: no request can claim it, and the field the proto reserves for delegation (`on_behalf_of`) is still not honoured.

A headless agent runs in its own trust zone — its own unix user or its own tailnet identity — and so has its own principal of kind `agent`. Every gate veto holds against it: it may not complete a gate, claim one, reclassify one, or unlink one, whether or not it filed it. The isolation that makes this true is the consumer's to provide; skift only reads what the transport gives it. An agent that shares a uid with a person is inside that person's zone by construction, and that is a deployment decision, not a policy bug.

The consequence is that the boundary between what a person may do and what an agent may do is drawn where the process is run, not by a field in a request. Stored delegation grants — a revocable, expiring record permitting a named agent principal to act on a user's authority — remain deferred; see [the roadmap](../roadmap.md) and [ADR-009](009-the-default-gate-policy-is-notification.md) for the gate policy this supports.

## Responsibility boundary

| Skift owns | Someone else owns |
|---|---|
| Tenancy scoping and policy-filtered lexical search | Compute isolation: microVMs, SELinux, capability drops |
| Un-fakeable provenance on every write | Egress control and credential proxies |
| Gate nodes and the human-visible ready stream | Supervision of agent behavior |
| Size caps on inline text and blobs | Moderation, and semantic retrieval and synthesis |
| Default policy: self and descendants, self-blocking only | Anomaly detection and rate limiting over the event stream |

Skift is not a sandbox, not a network boundary, and not a misalignment detector.

## Rejected Options

### Ordinary tracker, safety later

- **Good**: Fastest to build.
- **Bad**: Provenance and tenancy are the two things that cannot be retrofitted. Every later control depends on them.

### Enforcement inside skift

- **Good**: One place to look.
- **Bad**: Limits cannot be set sensibly before production. Counters and thresholds are policy, and they break "ready is dumb." It turns the store into a monitoring product.

## More Information

Supporting evidence: METR and Redwood Research, ["Brief independent investigation of agents' behavior, reasoning and collaboration in the OpenAI / Hugging Face hacking incident"](https://metr.org/blog/2026-08-26-openai-hugging-face-incident-investigation/), published 2026-08-26. A summary for skift's purposes, with the figures cited above, is in [docs/research/metr-openai-hugging-face-incident-2026.md](../research/metr-openai-hugging-face-incident-2026.md). Related: [ADR-002](002-connect-rpc-is-the-only-door.md) for transport-resolved identity, [ADR-004](004-embedded-opa-authorization.md) for where the default policy lives, [ADR-003](003-artifacts-are-content-addressed.md) for size caps. Revisit when the first rate limit or counter is needed; that is the signal that consumers have enough data to set one.
