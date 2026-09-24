---
status: accepted
date: 2026-09-06
---

# The default gate policy is a notification channel, not a safety boundary

## Context and Problem Statement

A gate is a node with `assignee_kind = user`: work only a person can perform. The shipped default policy vetoed the principal that filed a gate from completing, dropping, reclassifying, or unlinking it, on the grounds that approval from the requester is no approval at all.

Identity is resolved by the transport, so over the unix socket every process one unix user runs is that user's principal. An agent working interactively under a user's uid *is* that user principal. The filer veto therefore refused the ordinary interactive handoff: the agent, acting as the user, files a gate for a token it cannot mint and releases; the user sits down to mint it and cannot complete the gate, because the policy cannot tell the user from the agent that shares the uid. In a one-person organization that is a deadlock rather than a safeguard.

What is the shipped default protecting, and against whom?

## Decision Drivers

- Identity is a trust zone, not an assertion: the uid or tailnet identity a process runs under is what the server knows about it
- The interactive case must not deadlock, and it is the common case in a small organization
- The headless case — an agent in its own trust zone with its own `agent` principal — is the case the gate exists for, and every veto must hold against it
- Policy is embedded Rego for exactly this reason: an organization with different needs edits it ([ADR-004](004-embedded-opa-authorization.md))
- Nothing may become client-assertable ([ADR-006](006-provenance-not-authority.md))

## Considered Options

1. Keep the filer vetoes unscoped and let the interactive user work around them
2. Scope the filer vetoes to non-user principals
3. Add stored delegation grants so an agent can complete a gate on a user's authority

## Decision Outcome

Chosen option: **scope the filer vetoes to non-user principals**. The default is a notification channel, not a safety boundary. It answers "an agent cannot do this; a person must" and makes the ask visible in the human-filtered ready stream. It does not attempt to separate two humans from each other.

The shipped set:

| Veto | Scope |
|---|---|
| Only a user principal completes a gate | every non-user principal. Without it `assignee_kind: user` is a label |
| An agent may not claim a gate | every agent. A claimed gate leaves the ready set and its lease renews itself, so a headless agent holding one hides it from the person it is waiting for |
| The filer may not complete or drop its gate | non-user principals only |
| The filer may not change its gate's `assignee`, `assignee_kind`, or `kind` | non-user principals only. This is the silent escape: flipping `assignee_kind` defeats the gate with no error |
| The filer may not unlink its gate | non-user principals only. Removing the edge routes around the gate just as silently |

Net effect: a user principal has full control of the gates it files. An agent principal cannot complete, claim, reclassify, or unlink any gate, whether or not it filed it. An organization wanting approval separated between two humans re-adds the unscoped filer rules in its own Rego, which is what embedded policy is for.

Every gate veto states why and what to do instead, because a refusal an agent reads is the next prompt it acts on: `only a user may complete gate 01J…: a person completes it; comment your recommendation on it and release your lease with reason gate`.

### Consequences

- **Good**: The interactive handoff works. A user completes the gate an agent filed under the user's own uid, which is the whole point of running the agent there.
- **Good**: Nothing an unattended agent can do changes. Every veto still holds against a principal of kind `agent`.
- **Good**: The default says what it is. A reader of the policy no longer has to infer approval semantics it does not implement.
- **Bad**: An organization that wants two-human approval separation must write Rego to get it. The default no longer supplies it accidentally.
- **Neutral**: The user's authority now depends entirely on the trust zone the agent runs in. That is stated in [ADR-006](006-provenance-not-authority.md) and is the property the transport already had.

## Rejected Options

### Keep the filer vetoes unscoped

- **Good**: One rule, no principal-kind special case; approximates approval separation for free.
- **Bad**: Deadlocks the interactive case with no way out short of a second human or editing the policy. It also conflates a gate as *approval* with a gate as *an act only a person can perform*; the default serves the second.

### Stored delegation grants

A user principal records a revocable, expiring grant to an agent principal; policy permits gate completion when a live grant covers the caller.

- **Good**: Serves the interactive case without weakening the gate for unattended loops, and records both principals on the event.
- **Bad**: A table, an RPC, a lifecycle, and a second authorization path, to solve a problem the trust zone already solves. Deferred on [the roadmap](../roadmap.md).

## More Information

Decided on gate `dagger#13`, whose comment records the decision as taken. Related: [ADR-006](006-provenance-not-authority.md) for the trust zone as the delegation mechanism, [ADR-004](004-embedded-opa-authorization.md) for where the policy lives and how an organization overrides it, [ADR-002](002-connect-rpc-is-the-only-door.md) for transport-resolved identity. Revisit when a deployment has agents that are neither a user's own processes nor their own trust zone — an ephemeral sandbox uid is the first such case.
