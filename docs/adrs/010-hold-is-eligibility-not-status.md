---
status: accepted
date: 2026-09-08
---

# Parking work is a hold on the graph, not a status and not orchestrator configuration

## Context and Problem Statement

Skift has no way to park work. `status` is only `open`, `done`, `dropped`; `ArchiveProject` is a one-way read-only door; priority is advisory ordering that skift derives nothing from. So "not this week" can only be said by inflating a priority, and "not this project" can only be said outside skift.

The orchestrator is the obvious place to say it, and [the domain model](../domain-model.md) invited that: *"Ordering across projects is the orchestrator's business, because only the orchestrator knows which projects matter this week."* But an orchestrator holding the roster of what matters puts a human decision — sweep everything aside except A, B and C — outside the project management system, where `skift ready`, a laptop session, and a second orchestrator all disagree with it.

Where does the decision that work is parked belong, and what shape does it take?

## Decision Drivers

- Intent is the human's job and skift is the store for it ([vision](../vision.md)); a roster of what matters this week is intent
- The failure the vision opens by naming is the true board state living somewhere other than the board
- Stored state is only what cannot be derived ([principle 1](../core-principles.md))
- `Ready` must make no scheduling decision ([principle 6](../core-principles.md)), and must not acquire a stage machine ([vision](../vision.md), "not a workflow engine")
- A parked subtree must stay parked when a child is filed under it tomorrow
- Work that vanishes from the ready set with no explanation is worse than work that is never parked

## Considered Options

1. A roster in the orchestrator's configuration
2. A hold node with `blocks` edges into the work being parked
3. A fourth `status` value, `paused` or `held`
4. A hold as a stored eligibility fact on a node and a project

## Decision Outcome

Chosen option: **a hold as a stored eligibility fact**, on the node or project it is applied to, inherited down the parent relation and derived on every read.

The line the decision rests on:

> Priority orders the eligible set. Hold decides what is eligible. Ordering is the orchestrator's; eligibility is the graph's.

The original sentence conflated the two. What an orchestrator genuinely owns is arithmetic — how a `p0` in a three-day spike interleaves with a `p2` in the platform project, given budget, capacity, and which models are free. Combining two project-scoped priority scales needs a rule skift has no basis to choose. Deciding that a project is not this week's bet is a different question, and it is not arithmetic.

Eligibility was already multi-axis. A leased node leaves the ready set without its status changing, and so does a blocked one. Hold is a third axis of the same kind, which is why it is not a status: a held node is plainly still `open`, and nothing about it has moved through a stage.

The shipped shape:

| | |
|---|---|
| Stored | `held_at`, `held_by`, `held_reason`, `hold_group` on `nodes` and on `projects` |
| Derived | A node is *effectively held* if it, any ancestor via `parent_id`, or its project carries a hold. Never written down |
| Who | User principals only. An agent under a person's uid is that person's principal ([ADR-009](009-the-default-gate-policy-is-notification.md)); a headless agent in its own trust zone is refused, and the refusal names the next move |
| Reads and writes | Unaffected. Held work is read, decomposed, commented on, and re-linked as usual. Only `Claim` and `ClaimNext` refuse |
| Focus | `Focus` and `ClearFocus` sweep the complement of a keep-set, server-side, over an organization or within one project |

Focus is a server RPC rather than a client-side expansion for two reasons: a complement sweep over a large workspace exceeds the per-patch operation ceiling, and a semantic operation implemented in each client drifts between client versions. It is declarative and replacing — calling it again in a scope retires the previous sweep in the same transaction — so exactly one sweep can be active per scope and `ClearFocus` needs no sweep id. That matters because a sweep outlives every session that observes it, unlike a lease, whose fencing token works only because it belongs to one live session. **State that outlives sessions must be discoverable, not carried.**

A sweep holds only what is not already held, and marks what it holds with a `hold_group`; a hold placed by hand carries none, and `ClearFocus` releases only its own group. So an epic parked in March for its own reason survives both a sweep declared today and the clearing of it, with no stacked holds and no reference counting.

### Consequences

- **Good**: What is live is in skift. Every client — `skift ready`, an interactive session, one orchestrator or three — gets the same answer, and the reason, the principal, and the time are on the record.
- **Good**: Priority goes back to meaning one thing. Without a hold, `p4` has to mean both "low value" and "not now", and the second meaning is what drives the inflation the priority section already anticipates.
- **Good**: An orchestrator keeps everything that is genuinely its own — interleaving, budget, model choice, concurrency — and holds no facts about the projects it runs.
- **Bad**: Holding a node parks its downstream cone, and the cone is not itself held. Holding one node can silently stall a large amount of work. `Hold` therefore reports the count it parks, and `Explain` on a stalled dependent traces to the hold rather than stopping at the blocker.
- **Bad**: A hold is stored state that nothing expires, so a stale hold makes skift quietly wrong about what is live. The reason is required, holds are events, and parked work is listable — but nothing collects them. An expiry is deliberately deferred until stale holds are observed rather than imagined.
- **Bad**: Readiness gains a recursive ancestor walk, where every other condition is a flat anti-join. It is bounded by the acyclicity guarantee the parent relation already carries and backstopped by the statement timeout the limits table already names.
- **Neutral**: Archive becomes the terminal point of this axis rather than a separate concept: a permanent hold that also makes the project read-only. That archiving has no inverse in the API is a pre-existing hole, unchanged by this decision.
- **Neutral**: A held gate stops notifying, because notifiers subscribe to *ready* user-assigned nodes. Parking an ask silences it, which is what parking means.

## Rejected Options

### A roster in the orchestrator's configuration

The work is kept out of the orchestrator's reach by a list of projects and per-project priority floors in its own config.

- **Good**: No skift change at all. Ships with the orchestrator, and every filter it needs (`min_priority`, `labels`) already exists on `Ready`.
- **Bad**: The decision lives where no human or other client will look for it, and there is no record of who made it or why. Two orchestrators, or an orchestrator and a person, disagree about what is live. This is the incumbent failure the vision exists to fix, reproduced inside skift's own consumer.

### A hold node with `blocks` edges

Parking is expressed with the graph it already has: an open node titled "on hold", with `blocks` edges into the work being parked, completed or dropped to unpark.

- **Good**: No schema change, no new concept, atomic in one patch, and the event log and `Explain` come for free.
- **Bad**: A `blocks` edge means a dependency, and parking is not one; overloading it makes `Explain` say something untrue. Worse, edges attach to the nodes they name, so a child filed under a parked epic tomorrow escapes the park. That is not an ergonomic wart — it is the feature not working.

### A fourth `status`

`status` grows `held` beside `open`, `done`, and `dropped`.

- **Good**: One column, already on every query, no new relation to derive.
- **Bad**: Status is the stage a node has moved through, and a parked node has not moved. It would make `open` mean two things, force every consumer's status handling to change, and put a stage machine in a store that says it is not a workflow engine. It also cannot express a held project, or inheritance down a subtree, without a second mechanism anyway.

## More Information

Related: [ADR-009](009-the-default-gate-policy-is-notification.md) for the trust zone that makes "user principals only" workable rather than a deadlock, [ADR-006](006-provenance-not-authority.md) for why an agent hiding work from other agents is the shape of risk being guarded against, [ADR-004](004-embedded-opa-authorization.md) for where an organization changes who may hold.

This decision revises two documents rather than only adding to them: [the domain model's](../domain-model.md) priority section no longer sends the roster to the orchestrator, and [principle 6](../core-principles.md) now distinguishes eligibility from ordering.

Revisit when stale holds are observed in practice, which is the argument for an expiring hold; or when a deployment wants a person to park work without being able to unpark someone else's, which is a policy question rather than a model one.
