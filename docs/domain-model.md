# Domain Model

The model behind the `skift.v1` API. This document is the input to the proto contract; the proto is authoritative once it exists.

## Node

A unit of work. The only entity a human or agent thinks about directly.

| Field | Notes |
|---|---|
| `id` | ULID, internal |
| `number` | Sequential within its project, starting at 1. Displayed as `#7`, or `oauth-spike#7` across projects. See [Keys](#keys) |
| `org_id`, `project_id` | Tenancy scope, present on every entity |
| `parent_id` | Optional. Single parent. Answers "what is this part of" |
| `kind` | Single-valued label. Skift never interprets it. See [Node kinds](#node-kinds) |
| `title`, `body` | Body is markdown and is the prompt an agent receives |
| `status` | `open`, `done`, `dropped`. Nothing else is stored |
| `priority` | `p0` to `p4`, default `p2`. Advisory, and comparable only within a project. See [Priority](#priority) |
| `assignee_kind` | `agent` or `user`, matching `Principal.kind`. Unset means agent-eligible, never nobody's problem. A user-assigned node is a gate |
| `assignee` | Optional specific principal |
| `labels` | Free strings for projection and filtering |
| `held_at`, `held_by`, `held_reason` | Set while the node is parked. See [Hold](#hold) |
| `hold_group` | The focus sweep that placed the hold, if a sweep did. Unset for a hold placed by hand |
| `created_at` | Explicit, not inferred from the ULID. Identifier layout is not a business contract |
| `context_id` | Opaque UUID minted by skift at creation. Travels unchanged to the orchestrator and the agent as the A2A context id or harness session id |

Derived, never stored: `blocked` (has an unfinished blocking predecessor), `held` (it, an ancestor, or its project carries a hold), `ready` (open, not blocked, not held, no live lease, no open children), `claimed` (has a live lease).

### Node kinds

Skift never interprets `kind`. It exists because a single indexed value is what a board groups by and what a frontend renders an icon for, which multi-valued labels cannot give you.

A default vocabulary ships so that consumers render the same thing without coordinating: `task` (the default), `bug`, `spike`, `chore`. Two kinds that look tempting are deliberately absent. An epic is a node with children, derivable rather than declared. A gate is `assignee_kind: human`, which is the mechanism that actually enforces it.

Consumers may use any other value. Skift validates nothing and behaves identically regardless.

### Priority

`p0` through `p4`, defaulting to `p2`. The scale is chosen because models are steeped in it and will use it without instruction, not because skift derives anything from it.

Priority is comparable only within a project. A `p0` in a two-day spike and a `p2` in the platform project say nothing about each other, and with projects this cheap and numerous a global priority sort would be actively misleading. Combining two project-scoped scales into one ranking needs a rule skift has no basis to choose, so skift doesn't try: a caller reading or claiming across projects gets a round-robin turn across every project with ready work instead, priority-then-age within each project's turn. Which projects matter this week is a different question: that is intent, it is recorded here as a [hold](#hold), and every client sees the same answer.

Skift uses it as the first sort key in `Ready` within a project and derives nothing else from it. Expect inflation, and note that the counter-pressure is not a rule in the store: a consumer that stops trusting the field can sort however it likes.

Projects carry a priority of their own, and skift never sorts by it. Storing it is worth doing because people want to record which project matters. Sorting by it would require skift to decide how a project's priority combines with a node's, and a `p0` node in a `p3` project against a `p2` node in a `p0` project is exactly the question two reasonable organizations answer differently. That combination is the orchestrator's.

Hold is what keeps this field honest. Without it, `p4` has to mean both "low value" and "not now", and the second meaning is what drives the inflation. With somewhere to record "not now", a priority can go back to meaning one thing.

At scale the signal that actually governs this is allocation rather than priority: how much of a finite weekly capacity a project may consume. That is the deferred budget work, it has a natural home in the cost rollups, and it does not collapse into a two-level sort nobody can reason about.

### State changes

`status` holds only `open`, `done`, and `dropped`. Every transition is an event carrying the acting principal and a reason, because who changed it is half the record and why is the other half.

- **Complete** carries the result. See [Completion and results](#completion-and-results).
- **Drop** carries a reason: superseded, not viable, no longer relevant, or free text.
- **Reopen** moves `done` back to `open` and is how review feedback re-enters the graph. It is a normal transition, not an exception.

Reopen is **not retroactive**. It does not un-complete descendants that already consumed the earlier result. Work that must be redone is reopened explicitly in the same patch, which keeps the rule simple and the intent visible.

A node whose children have all closed becomes ready and is claimable. That is deliberate: the natural work at that point is synthesis or review of what the children produced.

## Project

Cheap to create and expected to be numerous and often short-lived. A project is exactly four things: an authorization scope, a namespace for node numbers, a rollup unit for cost, and a lifecycle unit. Keeping it to those four is what keeps it free. It also carries one annotation that is none of the four: the URL of the repository the project delivers to, stored for the orchestrators that work the graph so that two of them and a person agree on where the code lives. Skift never reads it — it neither clones nor checks that the host answers — and validates nothing beyond "an absolute URL, or empty".

A project is an authorization *scope*, not a unit that requires a grant. It inherits its policy from the organization, and diverges only when someone deliberately restricts it. If admission were required per project, the ritual removed from creation would reappear at the door.

| Field | Notes |
|---|---|
| `id` | ULID, immutable, internal |
| `org_id` | Tenancy scope |
| `slug` | Free-form alias derived from the title, disambiguated on collision. Mutable; retired slugs keep resolving |
| `title` | Human-readable name |
| `node_counter` | Next node number. A column, not a sequence |
| `archived_at` | Archiving is a deliberate act, not a timer. Keys stay valid forever |
| `held_at`, `held_by`, `held_reason`, `hold_group` | Set while the project is parked. See [Hold](#hold) |
| `priority` | Advisory. Skift never sorts by it |
| `vcs_url` | Clone URL of the repository the project delivers to, or empty. An annotation for orchestrators; skift never reads it. Set at creation or by `UpdateProject` |

Creating a project requires no provisioning, no DDL, and no name adjudication.

Abandonment, not archival, is how most short-lived projects end, and nothing reclaims them. A failed spike often holds the most useful record in the store: what was tried and why it did not work. Listing defaults to activity order rather than to an archived filter, and finding old work is retrieval's job rather than lifecycle's.

Archiving is the terminal point of the axis a [hold](#hold) occupies: a permanent park that also closes the project to writes. An archived project is read-only: no claims, no patches, no new edges into or out of it. Existing edges keep their meaning, so an archived node that was never completed still blocks whatever depended on it, which is visible through `Explain` rather than silently stalling. Archived projects drop out of listing and search by default.

**A project is not a node.** The tempting unification is that everything is a graph, so a project should be a root node with sub-projects as children. It should not. The project id is the indexed tenancy column on every query, and making it a graph traversal costs both performance and the isolation property [ADR-006](adrs/006-provenance-not-authority.md) rests on. Nesting also gives a node two candidate number namespaces.

## Keys

A node is referred to in prose by its project slug and its number: `oauth-spike#7`, or bare `#7` inside a project, or `acme/oauth-spike#7` fully qualified. The ULID stays internal and the API accepts either form.

Numbers are sequential within a project and start at 1, so a short-lived project stays in single or double digits. The counter is a column on the project row, allocated a block at a time within a patch. It is not a PostgreSQL sequence: projects are meant to be free to create, and per-project DDL would make them expensive.

A project's slug is free-form and unlimited in count, bounded to 100 characters and normalized to lowercase, derived from its title and disambiguated on collision. There is no scarce prefix namespace to adjudicate, which is what makes creating a project ritual-free. The slug is an alias, not an identity: projects carry an immutable id, renaming is allowed, and retired slugs keep resolving.

**Nodes never move between projects.** A number means nothing outside its namespace, so moving a node would renumber it and break every reference already written into a commit message, a comment, or an agent's memory. Reparenting within a project is fine. Across projects, create a new node and link the old one.

Numbers are dense within a project, so a mistyped or fabricated number can resolve to the wrong node. Resolution returns the node's title so a caller can check itself. The project half of a reference is sparse, so cross-project fabrication fails outright.

## Edge

| Field | Notes |
|---|---|
| `from`, `to` | Node ids. `from` blocks `to` |
| `kind` | `blocks`. The only kind that affects readiness. Others may be added as annotations |

A `done` or `dropped` predecessor satisfies a `blocks` edge, except for gates. See [Gates](#gates).

Edges may be removed; removal is an event.

### Acyclicity over the combined wait relation

Cycles are rejected at write time across the whole patch, and the relation checked is the union of two things:

- `blocks` edges
- the implicit wait from a parent to each of its children, because a parent is not ready while a child is open

Checking only the edge table is insufficient. A child `C` under parent `P`, plus an explicit edge from `P` to `C`, contains no edge cycle and can never become ready. The parent relation needs its own acyclicity guarantee for the same reason: containment that loops breaks rollups and the open-children rule.

The `kind` column stays even while `blocks` is the only kind implemented. The case it exists for is alignment: work that contributes to a longer-lived goal without blocking anything. Collapsing `kind` off the table for simplicity would foreclose that, and conflating a contribution with a dependency is how trackers acquire their reputation.

## Readiness

A node is ready when all of the following hold:

- `status = open`
- no `blocks` edge into it whose source is `open`
- no live lease on it
- no open children
- no hold on it, on any ancestor, or on its project
- the caller's filters match (projects, kind, assignee kind, labels, minimum priority)

The children rule is what makes an epic behave without anyone declaring one. A node that groups work is not itself work, and without this rule it sits in the ready set waiting for an agent to claim it and discover there is nothing to do. Since an epic is derived from having children rather than declared as a kind, the readiness rule has to be derived the same way. It is one more anti-join on `parent_id`.

`Ready` orders by priority then age within a project. Priority is project-scoped, so a cross-project ordering would be arithmetic on incomparable scales; instead, a filter naming several projects (or none, meaning every project the caller can read) gets a round-robin turn across the projects with ready work, one node per project per pass, rotating per principal and per filter so repeated calls advance rather than repeat. Cost and estimate rollups attach on request. `ClaimNext` performs `Ready` and `Claim` in one round trip using row locking with skip-locked semantics.

### Why not ready

Readiness is derived, so un-readiness is derivable too: the same anti-join, un-negated. `Explain` answers it for one node as blocked by these open nodes, leased until this time, parked by this hold, or waiting on a person.

This exists because of node limbo. A node disappears when it is blocked by something that will never complete, assigned to a human who never looks, leased by an orchestrator that died, parked by a hold nobody remembers placing, or sitting in a project everyone forgot. Most of those are visible once you can ask. The one worth detecting on its own is a node with no possible path to readiness, which is always a bug rather than a state.

## Hold

A stored decision to park work. A held node leaves the ready set and nothing else about it changes: its status is still `open`, its blockers are still satisfied or not on their own terms, and it returns the moment the hold is released.

Hold is an eligibility axis, not a stage. A lease already removes a node from the ready set without touching its status, and so does a blocking predecessor; a hold is the third fact of that kind. This is why it is not a fourth `status`: a parked node has not moved through anything. See [ADR-010](adrs/010-hold-is-eligibility-not-status.md).

**Containment carries a hold.** A node is *effectively held* when it, any ancestor through `parent_id`, or its project carries one. Holding an epic that did not hold its children would hold nothing claimable, and a child filed under a parked epic tomorrow must be parked too. The inheritance is a walk at read time, never a flag written down the subtree.

**A hold blocks claiming, not writing.** Held work is read, decomposed, commented on, re-linked, and re-estimated as usual; only `Claim` and `ClaimNext` refuse, naming the hold and the next move. This is the whole difference between a hold and archiving, which makes a project read-only.

**A held node still blocks its dependents.** Parking one node stalls everything downstream of it, and the cone is not itself held. `Hold` reports the number of nodes it parks, and `Explain` on a stalled dependent traces to the hold rather than stopping at the blocker.

**Only user principals may hold.** Parking work is a person's call, and a hold is also a way to hide work from other agents. Identity is a trust zone, so an agent running under a person's uid is that person's principal and holds with their authority; a headless agent in its own zone is refused, and the refusal names what to do instead. See [ADR-009](adrs/009-the-default-gate-policy-is-notification.md) and [ADR-006](adrs/006-provenance-not-authority.md).

Every hold and release is an event carrying the principal and the reason. A reason is required, because a hold nobody can explain is one nobody dares release.

### Focus

Saying "only these projects this week" is the complement of a hold, and it is one call: `Focus` takes a keep-set and holds everything outside it; `ClearFocus` releases what that sweep held.

Focus is server-side. A client computing the complement itself would exceed the operations-per-patch ceiling on a large workspace, and a semantic operation reimplemented per client drifts between versions.

Its scope is an organization, where the keep-set is projects, or a single project, where the keep-set is nodes. The two compose: an organization-wide focus says which projects are live, and a project-scoped one refines it to part of the graph.

Focus is declarative and replacing. Calling it again in a scope retires the previous sweep in the same transaction, so exactly one sweep is ever active per scope and `ClearFocus` needs no sweep id. That matters because a sweep outlives every session that observes it, unlike a lease, whose fencing token works only because it belongs to one live session: **state that outlives sessions has to be discoverable, not carried.** A sweep is discoverable because it is nothing but its holds, grouped by `hold_group`.

A sweep holds only what is not already held. A node parked by hand carries no `hold_group`, and `ClearFocus` releases only its own group, so a hold placed months ago for its own reason survives both a sweep and the clearing of it. There are no stacked holds and nothing to reference-count.

A project created after a sweep is live, not held. It was created because someone cared about it.

## Lease

The contract between skift and whatever executes work.

| Field | Notes |
|---|---|
| `id` | ULID. The fencing token. Returned by `Claim` and required on every later write |
| `node_id` | One live lease per node |
| `holder` | The principal that called `Claim` |
| `on_behalf_of` | Optional principal the holder is acting for. Not in v1; see [Delegation](#delegation) |
| `harness` | Free string such as `claude-code`, `hive` |
| `agent_ref` | Where this attempt ran. An A2A agent card URL, or whatever the harness identifies as |
| `session_ref` | Optional metadata: the harness session when one session works several nodes |
| `expires_at` | TTL requested per claim, bounded by the server |
| `release_reason` | On release: `complete`, `gate`, `crash`, `budget`, `expired`, `preempted` |

### Fencing

`Claim` returns the lease id, and `Heartbeat`, `Complete`, `Release`, and any patch touching the node must carry it. The server checks in the same statement that the lease is the node's current one and has not expired.

Without this, checking only the holder principal is not enough: an orchestrator whose lease expired and was reclaimed shares a principal across both attempts, so a late `Complete` from the abandoned attempt would succeed against work someone else now owns. The failure is silent and produces a wrong result, which is why the token exists from the first migration rather than later.

TTL is requested by the client and bounded by the server, because an unbounded TTL defeats expiry entirely.

Any write to the node by the holder renews the lease. `Heartbeat` renews explicitly. A systemd timer expires stale leases; expiry is an event and the node returns to the ready set. A node accumulates many leases over its life, one per attempt. Every attempt shares the node's context id, so a retry or a re-dispatch after review feedback resumes the prior session rather than starting over. A fresh slate is a new node.

### Placement and resume

Session state lives in neither skift nor the orchestrator. It lives where the work ran: an A2A agent keeps history per context, a Claude Code transcript sits on one host under one user, a worktree is a directory on one machine. A context id is therefore only resumable by the agent that owns it.

Skift records placement and never routes. `agent_ref` on each lease says where the last attempt ran, and lease history is queryable, so any orchestrator can look before dispatching. Routing is then an affinity preference rather than a requirement:

| Resume | Requires | Fidelity |
|---|---|---|
| Transcript | The same agent, reached with the node's context id | The full prior window |
| Graph | Any agent with the required skills | Node body, prior results, comments, artifacts |

Graph resume is what keeps orchestrators interchangeable, and it is why results and comments live on the node rather than only in an agent's head. It is also the version a human can read.

### Delegation

`on_behalf_of` records that a trusted holder acted for another principal, which is what a sandboxed agent returning a patch through its orchestrator needs.

It is **not in v1**. A field the client asserts is not client-assertable provenance by definition, so honoring it requires a server-known grant naming who may delegate, for whom, and within what scope. With one organization reaching skift over a unix socket, the orchestrator is the principal and the field has no use yet. It is defined here so the event and lease shapes do not change when it arrives.

`release_reason` exists so an orchestrator can tell a gate from a crash from a budget stop, and decide whether waiting for the previous agent is worth it. For coding agents, naming the worktree by context id makes the workspace follow the same affinity rule with no extra bookkeeping.

## Patch

The unit of mutation. A flat list of operations applied in one transaction:

- `create_node`, `update_node`, `drop_node { reason, cascade { max_nodes } }`, `complete_node { result }`, `reopen_node { reason }`
- `hold_node { reason }`, `unhold_node { reason }`
- `add_edge`, `remove_edge`
- `attach_artifact`, `add_comment`

Every patch carries a client-supplied `request_id`. A repeated request id returns the original result; the same id with different content is an error rather than a silent replay. Cycle detection runs over the resulting graph, not per edge.

### Concurrency

Applying a patch in one transaction is necessary and not sufficient. Under snapshot isolation two concurrent patches, one adding an edge from A to B and one adding an edge from B to A, each observe an acyclic graph and both commit. PostgreSQL permits that below `SERIALIZABLE`, and the result violates the invariant everything else rests on.

Patches that mutate edges or parents take a transaction-scoped advisory lock keyed on the organization. Patches that only create or update nodes do not. This is chosen over `SERIALIZABLE` with retry because it is trivially correct and needs no retry path; if it ever contends, that is measurable and the lock can narrow to a connected component.

`ClaimNext` participates in the same protocol. A readiness-changing mutation against a leased node is rejected, except for the holder's own gate-and-release.

### Authorization

A patch is authorized **inside its transaction, after resolution**, not in an interceptor. Aliases must be resolved and cascades expanded before the effective write set is known, so an interceptor cannot see which resources a patch actually touches. One denial rolls the whole patch back. The interceptor still performs authentication and coarse method-level checks.

### Forward references

An agent decomposing work creates several nodes and links them in one patch, so operations must be able to refer to nodes that do not exist yet. `create_node` takes a client-supplied `alias`, and any later operation in the same patch may use that alias wherever a node reference is expected. The server resolves aliases when it applies the patch.

Without aliases, decomposition needs a round trip per node, stops being atomic, and cycle-checks one edge at a time. This is the single constraint the patch format exists to satisfy.

### Shape

The format is written for a model to generate correctly on the first try, and secondarily to be cheap in tokens.

- **Flat, not nested.** A list of operations is easier to generate and validate than a tree, and it keeps one representation for creation and for later mutation.
- **Project hoisted to the patch.** Operations do not repeat it. Cross-project references are qualified.
- **`parent` is a field, not an edge.** Containment and dependency are different relations.
- **`blocked_by` on `create_node`** takes a list of references and is shorthand for the `add_edge` operations a linear chain would otherwise need.

### Drop semantics

Cascade carries the intent, and the two cases are different assertions.

- **Without cascade**, the drop asserts that downstream work does not need this after all: the dependency was unnecessary or is no longer a blocker. The node satisfies edges out of it and downstream becomes ready if nothing else blocks it.
- **With cascade**, the drop asserts the functionality is gone, and the subtree of nodes it blocks is dropped in the same patch.

Choosing wrongly is the agent's error, not an ambiguity in the model. Gates are the one exception: a dropped gate never satisfies anything.

A cascade is wide and quiet, so it states its blast radius before it runs: `cascade { max_nodes: N }`. The server counts the subtree the drop would take — the node named included, nodes already dropped excluded — and refuses the patch, naming the count and the bound, when the count is larger. Nothing is written. A caller that has not read the graph cannot state a truthful bound; one that has loses nothing.

A drop is reversible: `reopen_node` takes a dropped node back to open, and edges survive a drop. Undoing one needs the list of what it took, so **every patch response carries the references of every node it dropped**, cascade or not, under every view. That list is what a reopen works from.

## Completion and results

`complete_node` carries a result: a markdown body under a size cap, plus zero or more artifacts. Results and artifacts of all blocking predecessors are the inputs a downstream agent assembles its context from.

## Artifact

Shaped after A2A: name, description, parts, metadata. A part is one of:

- **text**: inline, under the size cap
- **blob**: a skift-managed, immutable, content-addressed object with a digest, media type, and size, stored by a configurable backend (filesystem, then object storage)
- **reference**: a URI with a digest to something that lives elsewhere. First-class schemes are `git` (branch, commit, pull request) and `oci` (arbitrary blobs in a registry)

Skift never stores raw bytes inside a node. Size limits are configurable per organization.

Digests are SHA-256, and a reference to something mutable records the resolved immutable identity: a git branch is stored with the commit it pointed at.

A digest proves byte integrity. It proves nothing about provenance or safety. Reference URIs are agent-supplied and therefore untrusted, so whatever fetches one is responsible for scheme and host restrictions and for refusing link-local and cloud metadata destinations. That responsibility sits with the consumer, since skift never fetches.

## Gates

A node with `assignee_kind = user`. Policy permits only user principals to complete it. An agent that needs a credential, a UI action, or a clarification files one and blocks itself on it.

**A gate is satisfied only by completion.** Dropping a gate never satisfies the edge out of it, unlike an ordinary node. Without that rule an agent blocked by its own gate could drop it and proceed, which turns the whole mechanism into a formality.

**An agent cannot dispose of the gate it filed.** Policy forbids a non-user principal that filed a gate from dropping, completing, reassigning, reclassifying, or unlinking it. The last three are what make it hold: flipping `assignee_kind` or removing the edge would route around the gate with no error. Every one of these refusals names the gate and the next move, because a refusal an agent reads is the next thing it acts on.

**A user disposes of its own gates.** The filer rules are scoped to non-user principals. Identity is resolved by the transport, so an agent running under a person's uid is that person's principal and acts with their authority; refusing it would refuse the person, and in a one-person organization that is a deadlock rather than a safeguard. A trust zone is the delegation: running the agent there is the grant, killing it is the revocation. An organization wanting approval separated between two humans re-adds the unscoped filer rules in its own Rego. See [ADR-009](adrs/009-the-default-gate-policy-is-notification.md) and [ADR-006](adrs/006-provenance-not-authority.md).

**Preparation is a separate node the gate depends on.** A gate is a node a human must perform: signing up for a service, minting a token, paying an invoice. The agent work that makes it easy is its own node, and the gate waits on it:

```
"Recommend a hosting provider"   →   "Sign up with the chosen provider"
   assignee_kind: agent               assignee_kind: user
```

Two nodes and one `blocks` edge. The agent claims the recommendation, completes it with its findings, and the gate becomes ready for the person with the recommendation as its inputs. The person arrives at a decision already researched instead of a bare ask. Claiming the gate itself would achieve nothing: an agent may not claim one, because a claimed gate leaves the ready set and its lease renews itself, hiding the ask from the person waiting for it.

Filing a gate and releasing the lease is one atomic operation. Doing it in two calls can strand a lease on a node whose graph has already changed.

A credential gate asks a person to perform an action or to place a secret where the agent's environment can reach it. It never carries the credential as a result, because results are durable and readable by everything downstream.

Notifiers subscribe to ready user-assigned nodes. A held gate is therefore silent: parking an ask is what parking means, and the hold is what `Explain` reports when someone asks why nobody was told.

## Usage

Append-only records of what work cost.

| Field | Notes |
|---|---|
| `node_id`, `lease_id` | Attribution. Per attempt, so a retry does not collapse into the original |
| `principal` | Who reported the actual, or who made the estimate |
| `model` | Free string |
| `input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_write_tokens` | Counts, never currency |
| `kind` | `actual` or `estimate` |
| `basis` | Estimates only. Free text: what the estimate was reasoned from |
| `created_at` | |

Estimates live here rather than on the node because they have an author and a history. A node re-estimated after a spike keeps both records, so "what did we think this would cost, and when did we change our minds" is answerable. A node's current estimate is the most recent one, derived rather than stored.

Currency is a rate table applied at query time. Subtree rollups of actual and estimate are queries. Budgets are attributes on a project or parent node; enforcing them is the consumer's policy, with the exception of a hard cap a human sets on a node, which skift checks at `Claim`.

## Principal

| Field | Notes |
|---|---|
| `id` | ULID, stable, internal. What every event and lease refers to |
| `org_id` | Tenancy scope |
| `kind` | `user`, `agent`, `service` |

Identity is separate from the principal, in a table keyed by scheme and value:

| Scheme | Value |
|---|---|
| `unix` | A uid on a host, from `SO_PEERCRED` |
| `tailscale` | A node identity from `WhoIs` |
| `oidc` | An issuer and subject |
| `token` | A bearer credential |

One principal may hold several identities, so the same person reaching skift over a unix socket and over the tailnet is one principal, and adding a transport is inserting a row rather than migrating. The identity used is recorded on each event, which answers "acted via which transport."

The internal id is deliberately not a URN. A URN bakes the scheme into the identity, which is the migration problem it would appear to solve. A URN is a reasonable display and interop form over the stable id.

Handlers never see the transport. Authorization is evaluated per request from principal, action, and resource by embedded Rego policy.

## Event

Immutable, append-only, time-partitioned. Every mutation writes one.

| Field | Notes |
|---|---|
| `id` | ULID. Time-ordered, so it doubles as the stream cursor |
| `org_id`, `project_id`, `node_id` | Scope |
| `principal_id`, `principal_kind` | The actor. Resolved by the transport, never client-supplied |
| `on_behalf_of` | Optional principal the actor acted for |
| `identity_scheme` | Which transport identity was used |
| `request_id` | The patch's idempotency key |
| `op` | The operation applied |
| `reason` | Why, for transitions that carry one |
| `before`, `after` | The change |
| `occurred_at` | |

Serves audit, agent context recovery, and the change notification stream. Whether a human principal has ever written to a node is derived from its events and exposed on the node.

## Comment

Content attached to a node, and its own entity rather than a kind of event.

| Field | Notes |
|---|---|
| `id`, `node_id` | |
| `author` | Principal |
| `body` | Markdown, under a size cap |
| `in_reply_to` | Optional. Threading |
| `resolved_at`, `resolved_by` | Optional. Set once; a comment without them is unresolved |
| `revisions` | Prior bodies with author and timestamp, retained |

Events are immutable by [principle 7](core-principles.md), and comments are the one thing here that people genuinely edit, thread, and resolve, so the two cannot share a table without special-casing. Three practical reasons stack on top: comments are indexed for search while events carry before-and-after blobs that should stay out of that index; event volume is far higher and wants different retention; and comments are the abuse surface [ADR-006](adrs/006-provenance-not-authority.md) names, so caps and policy on them should not become caps and policy on the audit log.

A comment is content. An event is the record of a change to content, including the change that created or edited the comment. Edits keep their prior revisions, so nothing is lost.

A reply is its own comment and is resolved on its own: resolving the comment it answers leaves it unresolved. A response that hands a node to whoever will work it carries a comments hint beside the inputs hint — how many of the node's comments are unresolved and when the newest of them was written — so the worker learns someone left a note without being handed every body.

## Change stream

Consumers of skift react to change: the notifier watches for gates, the retrieval service indexes new content, moderation inspects what enters the backlog, an auditor archives. They all want the same thing, so there is one stream.

`WatchEvents` takes a cursor and returns events after it, filtered by project and operation, scoped to the caller's organization, and policy-filtered so a consumer sees only what its principal may read. A consumer that dies resumes where it stopped.

The cursor is opaque, and it is **not** simply the last event id. A time-ordered ULID is allocated before commit, so a transaction can take a low id, stall, and commit after a higher id has already been delivered; a consumer that advanced past it would skip it permanently. The cursor therefore has to carry a commit-ordered position, which in PostgreSQL means a snapshot-aware boundary below which every transaction is known to have committed. This is deferred with the stream itself, and it is written down here so the naive version does not get built.

PostgreSQL `LISTEN`/`NOTIFY` is the wake signal, not the transport. The notification says something changed in an organization; the consumer then reads from the events table by cursor. Payload limits and dropped notifications on reconnect make `NOTIFY` unreliable as a carrier, and this arrangement is durable and replayable regardless. Delivery is at-least-once and consumers deduplicate by event id.

Events are time-partitioned, and **audit retention and delivery replay are different windows.** The audit record is kept indefinitely, which is what [principle 7](core-principles.md) means. The window a consumer may resume from is bounded, and a cursor older than it is rejected so the consumer resynchronizes rather than silently skipping.

### Orchestrators do not use it

Readiness is derived, so a node becoming ready produces no event of its own: it became ready because a *different* node completed. Emitting derived readiness events would mean storing derived state, which [principle 1](core-principles.md) forbids, and it would drift.

Orchestrators therefore do not watch a stream. They call `ClaimNext` with a wait, which blocks until work matching their filters is available and returns it already claimed. An orchestrator wants work, not notifications, and this is both simpler and free of the drift problem.

## Search

Lexical search over node titles, bodies, results, and comments, scoped to an organization and filtered by policy. PostgreSQL full-text with a GIN index. It belongs in skift because policy filtering has to happen where the rows are, and because free projects trade a curation problem for a discovery problem: with hundreds of short-lived slugs, search is how anything is found again.

Semantic retrieval, hybrid ranking, and synthesized answers are not skift's. They need embeddings, chunking, a model, and a queue, and they belong to a retrieval service consuming skift's event stream, in the same way as the notifier and moderation. The [kb](https://github.com/ivy/kb) service already has that machinery and its principle "KB indexes, repos own" carries over unchanged.

## Default policy

The default Rego policy for an agent:

- May **file work with no lease at all**: create a node at the root of a project, or under a lineage it created itself. It may also block what it filed on anything it can see in the project, and unblock it again. Filing is how an agent turns intent into a graph, and it is not mutation; changing a filed node afterwards needs its lease like any other write
- As a lease holder, may mutate **its own node and the children it creates under it**, and nothing else. Not arbitrary descendants
- May add `blocks` edges into its own node
- May not add edges out of its node without link permission on the target
- Cross-project edges require write on the source and link permission on the target
- May not dispose of a gate it filed. See [Gates](#gates)
- May not hold or release anything. Parking work is a user's call; an agent that thinks work should be parked says so in a comment. See [Hold](#hold)
- Anything else is rejected; the agent files a gate node to ask

Policy is evaluated on the resolved patch inside its transaction. See [Authorization](#authorization).

## Limits

Fixed structural ceilings, distinct from adaptive rate limiting. These need no production data to set, because they exist to stop one request from consuming the service rather than to shape behaviour. Set them high enough that ordinary work never meets them.

| Limit | Why |
|---|---|
| Operations per patch | One transaction should not hold a lock unboundedly |
| Nodes and edges created per patch | Same, and it bounds cycle-check work |
| Body, result, and comment size | Storage, and the abuse ceiling from [ADR-006](adrs/006-provenance-not-authority.md) |
| Inline part size, managed blob size | Skift is not a data transport |
| Statement timeout | A pathological recursive query cannot stall the service |
| Lease TTL ceiling | An unbounded TTL defeats expiry |
| Page size | Unbounded reads are a denial of service with extra steps |
| Keep-set size in a focus sweep | One call should not rewrite an organization unboundedly |

Per-principal rate limiting is **not** here. Realistic limits cannot be chosen before observing real workloads, and the interceptor is the seam for adding them when they can.

## Content is data, not instructions

Node bodies, comments, results, and artifacts are written by agents. A downstream agent assembling context from its predecessors is concatenating agent-authored text into a prompt, which is the threat [ADR-006](adrs/006-provenance-not-authority.md) describes arriving through the front door rather than through a covert channel.

Skift stores and attributes this content and never interprets it. The obligation on a consumer is to render it as attributed, delimited data rather than folding it into instructions, and to treat a predecessor's result as a claim by that principal rather than as a fact. Provenance is what makes that possible, which is the practical reason every write carries an actor.

See [ADR-006](adrs/006-provenance-not-authority.md).

## Open questions

- Field-level masking for PII and PHI in policy responses. Deferred.
