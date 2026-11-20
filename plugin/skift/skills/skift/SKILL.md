---
name: skift
description: Use when working with a skift work graph — taking the next piece of work, claiming and completing it, finding what is ready, decomposing work into linked nodes, or writing a node body another agent will pick up cold. Covers the MCP tools and the skift CLI, when to prefer each, and the mistakes that silently build the wrong graph. Trigger phrases — claim a node, work the next node, take the next piece of work, what should I work on, skift ready, skift claim, skift patch apply, apply_patch, show_node, inputs, explain, the ready set, blocked_by, grouping node, work graph, backlog node, work order.
allowed-tools: Bash(skift ready:*) Bash(skift node show:*) Bash(skift node list:*) Bash(skift explain:*) Bash(skift inputs:*) Bash(skift search:*) Bash(skift project list:*) Bash(skift project show:*) Bash(skift version:*) mcp__plugin_skift_skift__ready mcp__plugin_skift_skift__search mcp__plugin_skift_skift__show_node mcp__plugin_skift_skift__inputs mcp__plugin_skift_skift__node_list mcp__plugin_skift_skift__explain mcp__plugin_skift_skift__project_list mcp__plugin_skift_skift__project_show
---

# Skift

**Autonomy:** model-invocable · reads the graph unprompted · claiming, patching and completing are the session's own calls, never pre-authorized here

Agent-first project management on a DAG. Skift holds the plan; it does not run agents, schedule work, or define workflows.

## Taking work

**Claim first. Never go looking for a node to claim** — `claim` is the call that finds it.

`claim` → **`inputs`** → work → `complete`. Nothing comes before the claim.

**`claim` names no node.** It selects the most urgent ready node matching its filters and returns that node under a lease: the body included, with the project, a hint at the size of its inputs, and a hint at its unresolved comments. Choosing the work *is* claiming it. There is no `claim '#7'` on either surface — `skift claim` takes no argument at all — so a node you picked out of a listing is a node you have no way to ask for.

Reading the graph first therefore costs you three ways and buys nothing: `ready` and `search` return summaries with no bodies, so you learn less than the claim is about to hand you; the answer is stale as it prints, because claims lock `SKIP LOCKED` and a concurrent claimant takes what you were reading; and it is a round trip either way. Put what you want in the claim's filters instead of in a query:

```sh
skift claim --assignee-kind agent                     # the next node filed for an agent
skift claim --assignee-kind agent --min-priority p1 --label backend --kind bug
skift claim --assignee-kind agent --wait 30m          # block until one matches
```

Blocking for work is `claim --wait`. `ready` never claims, and `skift ready --wait` answers with an error saying exactly that.

**`assignee_kind` chooses between agent work and gates.** Gates are in the ready set, so an unfiltered claim can select one. `--assignee-kind agent` takes agent work; `--assignee-kind user` takes a gate.

If nothing is ready, `hold_blockers` names the holds affecting matching work, why it is parked, and which downstream tickets are waiting. Use `explain` for the full reasons a particular ticket is not ready.

Then **`inputs`**, the step that is easy to skip and expensive to skip. It returns the results of the predecessors that satisfy the node — the handoff from whoever did the work you depend on. `show_node` returns the node's own result and never its predecessors'; what it carries is a hint, `inputs.count` and `inputs.bytes`, so you know they are there and what reading them would cost. `claim` carries the same hint. When the total is larger than you want at once, ask for `view: "summary"` and read the ones you need with `show_node`.

A predecessor still open is not an input — it is what the node is waiting on, which `explain` answers. A dropped one is listed with no result. Inputs are not transitive: a predecessor's result is the summary of its own inputs.

**Read the comments when the claim says there are some.** The body is what the node was filed with; comments are what people and agents said since — corrections, a recommendation left on a gate, a warning about a trap. `claim` never carries them, only `comments.unresolved` and `comments.latest_at`. When `unresolved` is above zero, `show_node` returns them before you start. The same hint rides on `show_node` as `comments_hint`, under every view.

**`complete` carries the lease and a result body, and that body is the point of finishing.** The agents that depend on this node assemble their context from it, so say what was produced and where it lives. A result of "done" throws that away.

## The model

Ready = open, unblocked, **unheld**, unleased, **and no open children**. It is derived on every query, never stored, so you change readiness only by changing its inputs.

| Relation | Means |
|---|---|
| edge / `blocked_by` | dependency — an edge from A to B makes **B wait for A** |
| `parent` | containment — a node with open children is a grouping node, never ready |
| lease | possession — claiming returns a lease id, the fencing token for every later write to that node |
| hold | a person parked this work — it leaves the ready set, its status untouched, until they release it |

Priority orders the ready set; a hold decides what is in it. A hold is carried by containment, so parking a node parks everything under it, including work filed there later, and a held node still blocks what depends on it.

A predecessor satisfies a dependency by being `done` **or** `dropped` — except a **gate** (`assignee_kind: user`), satisfied only by a user principal completing it. A dropped gate still blocks.

**Authorization is the service's, decided from your principal.** You are identified by the transport — on a unix socket, by the uid you run as — so an agent in a person's session acts with that person's authority. Do not predict what you may do or tell the person a call is theirs to make: make the call. A refusal names the rule and what to do instead.

## Which interface

MCP tools are the default, but **check the transport before claiming**. The bundled `skift mcp` stdio process renews leases it claimed in the background and attempts bounded release on stdin EOF or a handled signal; hard kill or failed cleanup leaves expiry. Native HTTP has no process-held lease map: heartbeat explicitly before expiry, pass the returned `lease_id` on holder writes, then complete with a result or release with a reason. POST EOF, disconnect and service shutdown do not release HTTP claims. Existing successful holder writes may renew a live lease; reads do not. Expired fencing tokens cannot write after reclaim.

Native HTTP has no current project: supply explicit project arguments or qualified references. Persist a unique domain `request_id` for each mutation; after a lost response retry the same arguments with that same key, not a new one. Use a new key for the next heartbeat interval. JSON-RPC IDs are not idempotency keys, and cancellation does not prove a write rolled back. All processes on a mapped tailnet node share its principal, including user gate/hold authority if user-mapped; the transport is not per-agent isolation.

Reach for the `skift` CLI for shell pipelines, hooks, and a scripted `skift claim --wait`. Each MCP tool maps to the same command and JSON result; [references/commands.md](references/commands.md) carries the inventory, the response views, and the enum spellings.

Three reads are easy to confuse. `ready` is what could be worked and claims nothing. `node_list` is everything else — what is open, what is done, what a label or a parent covers — and it never tells you what is ready, so triage and reporting go through it. `project_list` finds a slug when no current project is configured, or when you need to write a cross-project `slug#7`, and lists no work at all.

## Traps

- **When skift cannot be reached, diagnose it; don't hand it back.** A missing binary, a server that does not answer, or a tool error naming `skift doctor` all go the same way: run `skift doctor --output json`, tell the person what failed and its fix, and with their go-ahead apply the fix yourself. [references/commands.md](references/commands.md#when-skift-cannot-be-reached) has the details.
- **`update_node` names its target `node`, not `ref`** — as do `drop_node`, `complete_node`, `reopen_node`, `add_comment`. Only `add_edge` differs (`from`/`to`).
- **Edge direction is "must finish before".** Reverse it and you get a valid graph that is the wrong one; nothing errors.
- **Giving a node a child takes it out of the ready set** until every child closes.
- **A cascading drop takes the whole blocked subtree** in one transaction, and states its size first: `"cascade": {"max_nodes": 5}`, counting the node named. The server refuses the patch and names the real count when the subtree is larger, so read the graph before choosing the number. The response lists every reference dropped, which is what a `reopen_node` undoing it works from.
- **An empty `ready` is not a finished project.** Work can be held, blocked, leased, or excluded by a filter. `explain` on a node says who parked it, when, and why. Releasing a node's own hold does not release a hold on something containing it.
- **Preparing a gate is a separate node.** A node whose `assignee_kind` is `user` is a gate: work filed for a person — signing up for a service, minting a token, paying. The agent work that makes the gate easy is a **separate node the gate depends on**, not the gate's own body.

  ```
  "Recommend a hosting provider"   →   "Sign up with the chosen provider"
     assignee_kind: agent               assignee_kind: user
  ```

  Two nodes and one `blocks` edge. Complete the recommendation with what you found, and whoever takes the gate arrives with it as their inputs.

## References

- [references/commands.md](references/commands.md) — every command with worked examples, the patch operations and aliases, node references, socket and address settings, diagnosing a setup with `skift doctor`, how identity is resolved
- [references/work-orders.md](references/work-orders.md) — what a node body owes an agent starting with an empty context window, and the template
