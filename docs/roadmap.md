# Roadmap

Milestones toward the [vision](vision.md), in dependency order rather than on a calendar. Each one is defined by what becomes possible, not by a date.

The rule that keeps this honest: **nothing moves to the next milestone until the current one is used daily.** Skift exists to remove a bottleneck, and a milestone that is not in use has not removed anything.

## M0 — Walking skeleton

One organization, one project, over a unix socket. An agent creates and links nodes in a patch, asks what is ready, claims it with a fenced lease, and completes it with a result. Every step is an event.

Includes the correctness work that is expensive to retrofit: lease fencing, patch-scoped concurrency control, cycle detection over the combined wait relation, authorization of the resolved patch inside its transaction, and fixed structural ceilings.

Excludes artifacts, search, comments, the change stream, cost tracking, tsnet, MCP, delegation, and anything with a second organization in it.

**Done when** an orchestrator completes a dependency chain end to end and the concurrency tests pass.

## M1 — Dogfood

In progress.

Skift holds its own backlog and the work of building it. Adds what a human needs to stay in the loop: gates with a notifier, `Explain`, comments, and lexical search.

This is the first milestone where the vision's human-escalation story actually works, and the first real evidence about whether the model fits real work.

**Done when** the flat-file backlog is retired because skift holds it instead.

## M2 — Orchestration

The hive successor dispatches from skift. Adds artifacts, usage records and cost rollups, `ClaimNext` affinity by `agent_ref`, and the MCP server so a laptop session is a first-class client.

**Done when** work reaches pull requests without a human choosing what runs next.

## M3 — Distribution

Agents on other machines. Adds tsnet, the change stream with a commit-ordered cursor, and the retrieval service as its first consumer. Cross-project work becomes ordinary.

**Done when** an agent on the laptop and an agent in grove work the same graph without either knowing where the other is.

## M4 — Product

The first deployment that is not the author's. Adds a second organization and everything that implies: OIDC, policy administration, field masking for sensitive data, adaptive quotas informed by real usage, retention and export controls, backup and restore guarantees, and a web UI.

Most of the [external review's](adrs/) deferred findings live here. That is deliberate: they are the cost of a second tenant, and paying it before there is one is how projects die before shipping.

**Done when** someone else runs skift and it survives.

## Deliberately unscheduled

- **Semantic retrieval and synthesis.** A separate program consuming the change stream, successor to [kb](https://github.com/ivy/kb). Blocked on M3, not on skift.
- **Supervision and moderation.** Consumers, not features. See [ADR-006](adrs/006-provenance-not-authority.md).
- **Budget enforcement.** Records exist from M2. Enforcement is the orchestrator's until someone proves otherwise.
- **Metering-grade usage.** Estimate-grade is best effort by design. Billing would change that, and there is no billing.
- **Stored delegation grants.** A revocable, expiring record letting a named agent principal act on a user's authority, with both principals on the event. The trust zone the agent runs in is the grant today, and it costs nothing; see [ADR-009](adrs/009-the-default-gate-policy-is-notification.md). Worth building when a deployment needs an agent to hold a user's authority without running inside the user's zone.
- **An identity mapping for ephemeral sandbox uids.** `internal/server/identity.go` refuses an unmapped uid as unauthenticated, so an agent in a throwaway sandbox has no principal until someone inserts one. Blocked on there being such a sandbox: the mapping has to follow how those uids are actually allocated.
- **A policy veto on large cascades.** A `drop_node` cascade already states the bound it fits within, and the server refuses a larger subtree; a Rego rule capping the bound itself for non-`user` principals would hold regardless of what a model was told. Worth having, not worth having yet.
