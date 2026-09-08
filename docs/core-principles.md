# Core Principles

Decision filters for dagger. Each one can be violated, and violating it should feel wrong.

## 1. The graph is the truth

Stored state is only what cannot be derived: a node's status (`open`, `done`, `dropped`), its edges, its leases, and its holds. Blocked, ready, claimed, and held are views computed from those facts. Rollups over a subtree are queries.

A hold is stored because a decision to park work follows from nothing else in the graph. Whether a node is *effectively* held, because it, an ancestor, or its project carries one, is derived on every read and never written down.

**Says no to**: A `blocked` column. A `ready` flag maintained by triggers. A hold propagated to descendants at write time. Any cached derivation that can drift from the facts that define it.

## 2. One door

The Connect API is the only way in. Agents, humans, orchestrators, the CLI, the MCP server, and the web UI are all clients of the same contract. The proto package is the domain model; the database schema is an implementation detail behind it.

**Says no to**: Handing any client a database credential. Exposing sqlc-generated types on the wire. A "quick" script that writes to Postgres directly.

## 3. Agents and humans are principals, not roles

A principal is a user, an agent, or a service. All principals use the same API. What differs is authorization policy, evaluated per request from the principal, the action, and the resource. A gate that only a human can pass is an ordinary node plus a policy rule.

**Says no to**: An agent-only API and a human-only API. Special-casing "bot" accounts. Workflow features that exist because policy could not express them.

## 4. Mutations are atomic and idempotent

A graph change is a patch: a list of operations applied in one transaction, cycle-checked as a whole, and deduplicated by a client-supplied request id. An agent that dies halfway leaves no half-graph. An agent that retries creates nothing twice.

**Says no to**: A create-node call followed by five add-edge calls. Cycle checks that pass per edge and fail for the batch. Any mutation without an idempotency key.

## 5. Dagger stores plans, not work

Dagger knows what should be done, what is ready, and who holds a lease. It does not know how work is done. The lease is the whole contract with an orchestrator: claim, heartbeat, release, complete. Whoever calls `Claim` owns the lease and keeps it alive.

**Says no to**: Dagger spawning, supervising, or messaging an agent. Dagger choosing which agent gets which node. Dagger holding a workflow definition.

## 6. Ready is dumb

`Ready` returns the set of eligible nodes, open and unblocked and unclaimed and unheld with no open children, filtered by what the caller asks for and sorted by priority and age. It carries the numbers a consumer needs, such as cost and estimate rollups, and makes no decision with them. Budget enforcement, model selection, and scheduling are the consumer's policy.

Eligibility and ordering are different things. Eligibility is a fact about the graph, and every client sees the same answer. Ordering is a hint: priority orders the eligible set and never gates it, and a consumer may take dagger's order or compute its own.

**Says no to**: Budget logic inside `Ready`. A scheduler in the store. An order that mixes priority scales from two projects. A default that two reasonable organizations would want to differ on.

## 7. Nothing is deleted

Drop is a state. Edges may be removed, and their removal is an event. Every change is appended to an immutable event log that serves audit for the organization and context recovery for the agent.

**Says no to**: Hard deletes. Mutable events. Rewriting history to tidy a board.

The audit record is kept indefinitely. The window from which a stream consumer may resume is bounded and separate; expiring a replay cursor destroys nothing.

## 8. Fit for one server, shaped for a thousand

Every table carries its organization and project from the first migration. Every query is scoped by them. Storage, transport, and identity are swappable behind interfaces. Nothing else is built for scale until scale demands it.

**Says no to**: Retrofitting tenancy. A query that can scan across organizations. Organization management, SSO, or billing before a second organization exists.

## 9. Provenance is not client-assertable, and the store never confers authority

Every write records the acting principal and its kind, resolved by the transport and never settable by a client. Delegation, where a trusted holder acts for another principal, requires a server-known grant rather than a claim in the request. Whether a human has ever touched a node is derivable and exposed. An assignment from one agent to another is a fact about the graph, not permission to act. Consumers decide what agent-created work is worth, and a gate node is how an agent asks for what policy does not let it do.

**Says no to**: Client-supplied author fields. Any feature where an agent's action grants another agent permission. Treating consensus among agents as approval. Content inspection as a substitute for attribution.

## 10. Adopt standards where they carry interoperability

Where an established protocol already names a concept dagger needs, dagger uses that shape: A2A's context id, artifact, and part; OCI for blobs that live outside dagger; Rego for policy. The cost of a translation layer is paid once at the boundary, not on every integration.

**Says no to**: A bespoke artifact format. A bespoke policy language. Reinventing a concept a standard already defines because dagger's version would be slightly neater.
