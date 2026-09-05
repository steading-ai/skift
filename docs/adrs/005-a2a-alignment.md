---
status: accepted
date: 2026-09-04
---

# Align with A2A: a dagger node maps to many A2A tasks, one per lease

## Context and Problem Statement

Agents in the surrounding fleet speak [A2A](https://a2a-protocol.org). A2A defines tasks, context ids, artifacts, parts, and task states including input-required and auth-required. Which of these does dagger adopt, and does adopting them change dagger from a pull-based store into a push-based dispatcher?

## Decision Drivers

- Interoperability across agent harnesses is a product requirement
- Dagger must not become an orchestrator or a workflow engine
- An A2A task is an interaction owned by one agent's execution; a dagger node is a durable plan worked across many attempts

## Considered Options

1. Dagger nodes are A2A tasks; dagger is an A2A server
2. Dagger is an A2A client that pushes work to agents
3. Dagger stays a passive store; orchestrators are the A2A clients; dagger adopts A2A shapes and ids at the boundary

## Decision Outcome

Chosen option: **passive store, orchestrators push, shapes adopted at the boundary**. A dagger node maps to zero or more A2A tasks over its life, one per lease. The node's context id, minted by dagger at creation, is the A2A context id: the orchestrator passes it on `SendMessage`, and every attempt shares it. Dagger stores no A2A task id; attaching to running work is `ListTasks` filtered by context id and then `SubscribeToTask` against the agent, never proxied by dagger.

Adopted from A2A:

- **Context id** as the one correlation key, owned by the node, spanning attempts
- **Artifact and part shape** so outputs flow from an A2A completion into dagger without translation
- **Input-required and auth-required** states as the trigger for an orchestrator to file a gate node

Not adopted: A2A's task lifecycle as dagger's node status, push notification configs as dagger's notifier, and any role for dagger as an A2A endpoint.

### Consequences

- **Good**: Any A2A-capable harness plugs into an orchestrator without dagger changes.
- **Good**: The gate mechanism works for every harness, not only Claude Code.
- **Good**: Dagger remains pull-based. Thundering herd on the ready set is bounded by row locking and an atomic `ClaimNext`, and a change stream replaces polling.
- **Good**: A retry, or a re-dispatch after human or reviewer feedback, resumes the prior session with its full context. A fresh slate is a new node.
- **Bad**: A session that works several nodes has several context ids; the harness session is optional lease metadata, not a structural id.
- **Neutral**: Whether a sandboxed agent talks to dagger directly or returns a proposed patch over A2A for the orchestrator to apply is a per-deployment policy choice. Both are supported through the acting principal and on-behalf-of fields.

## Rejected Options

### Dagger as A2A server

- **Good**: Agents could address dagger like any peer.
- **Bad**: Conflates a durable plan with an ephemeral interaction. Dagger would need to speak task lifecycle it does not own.

### Dagger as A2A client

- **Good**: Push could reduce polling.
- **Bad**: Dagger would choose which agent runs what. That is orchestration, and it becomes a workflow engine.

## More Information

Related: [ADR-003](003-artifacts-are-content-addressed.md) for the artifact shape. The thicket project's principle that agent-to-agent work goes over A2A and that bridges hold no policy is the counterpart to this decision.
