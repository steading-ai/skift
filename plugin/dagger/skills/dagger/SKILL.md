---
name: dagger
description: Use when working with a dagger project graph — finding what is ready, claiming and completing work, decomposing work into linked nodes, or writing a node body another agent will pick up cold. Covers both the dagger MCP tools and the dg CLI, when to prefer each, and the mistakes that silently build the wrong graph.
---

# Dagger

Dagger is agent-first project management on a directed acyclic graph. A project
is a graph of work; a node becomes ready when its dependencies are satisfied,
and ready work can be claimed and run in parallel. Dagger holds the plan — it
does not run agents, schedule work, or define workflows.

## The model

A project is a graph of work. A node is ready when it is open, unblocked,
unleased, and has no open children. Readiness is derived, never stored — you
cannot set it, only change what it is derived from.

Three relations matter:

- **`blocked_by` / edges** are dependency. An edge from A to B means A must
  finish before B is ready.
- **`parent`** is containment. A node with open children is a grouping node and
  is never ready; it becomes ready when its last child closes.
- **A lease** is possession. Claiming a node returns a lease id, and that id is
  the fencing token every later write to the node must carry.

A predecessor satisfies a dependency by being `done` **or** `dropped` — with one
exception: a **gate** (`assignee_kind: user`) is satisfied only by a user
completing it. A dropped gate still blocks, so an agent cannot drop its own gate
and proceed. Reopening a node does not reopen its descendants.

## Which interface

Both speak to the same service and take the same node references.

**MCP tools** (`ready`, `search`, `show_node`, `explain`, `claim`, `heartbeat`,
`release`, `complete`, `comment`, `apply_patch`) are the default in a harness
session. A lease claimed through a tool is renewed in the background and
released with reason `crash` if the session ends; a hard kill leaves it to
expire server-side.

**The `dg` CLI** is the same client. Reach for it when:

- you are writing more than two or three nodes at once — `apply_patch` echoes
  every created node *and* every event back to you, which is expensive in a
  conversation, while `dg patch apply - < patch.json` returns the same result
  you can summarise with `jq`
- you are in a shell script or a hook
- you want `dg ready --wait`, which blocks until work exists and returns it
  already claimed

Output is JSON whenever stdout is not a terminal, and on request with
`--output json`. Errors are a JSON object on stderr: `{"code":…, "message":…}`.

## Finding and reading work

```bash
dg ready                      # what can be worked now, most urgent first
dg ready --assignee-kind user # what is waiting on a human
dg node show '#7'             # one node in full: edges, result, lease, comments
dg explain '#7'               # why it is not ready; no reasons means it is
dg search 'socket "permission denied"'   # websearch syntax, across projects
```

Read the node before starting it. `show_node` gives you the body, which is the
work order, and the edges, which tell you what its inputs were. If it is
blocked, `explain` returns structured reasons rather than prose.

## Taking and finishing work

```bash
dg claim --min-priority p2            # returns the node AND a lease id
dg claim --wait 30m --kind bug        # block until something matches
dg complete '#7' --lease "$LEASE" --result 'What was produced, and where.'
dg release "$LEASE" --reason gate     # hand it back: filed a gate, waiting on a human
```

The result body is what downstream agents assemble their context from. Say what
was produced and where it lives; a result of "done" wastes the graph.

**Never claim a node whose `assignee_kind` is `user`.** That is a gate: a
decision a human owes, deliberately blocking something. Filter with
`--assignee-kind agent` if you might otherwise.

## Changing the graph

`apply_patch` is the only way to mutate the graph. A patch is a list of
operations applied in one transaction and cycle-checked as a whole — if any
operation is rejected, nothing in the patch happens.

Operations: `create_node`, `update_node`, `drop_node`, `complete_node`,
`reopen_node`, `add_edge`, `remove_edge`, `attach_artifact`, `add_comment`,
`edit_comment`, `resolve_comment`.

**Aliases are what make one patch a whole subgraph.** A `create_node` declares
an `alias`, and any later operation in the same patch refers to the new node by
it — no round trip to learn its number:

```json
{"ops": [
  {"create_node": {"alias": "spike", "title": "Spike the parser", "kind": "spike"}},
  {"create_node": {"alias": "impl", "title": "Implement it", "blocked_by": ["spike"]}},
  {"create_node": {"alias": "docs", "title": "Document it", "parent": "impl"}}
]}
```

The response maps every alias to the reference it became.

For a single small change the CLI has shortcuts that build the patch for you:

```bash
dg create 'Title' --body - --kind bug --priority p1 --blocked-by '#3'
dg link '#3' '#7'          # #3 must finish before #7 is ready
dg comment '#7' 'note'
```

## Writing a node worth claiming

A node body is a **work order for an agent starting with an empty context
window**. It has none of your conversation. A body of one or two sentences is
ambiguous by the time anyone reads it.

Structure that works:

````markdown
## Context
What is true today, why this exists, and which documents govern it.

## Deliverables
Concrete paths and artifacts, not intentions.

## Acceptance criteria
- [ ] Testable statements, each with the command that tests it

## Scenarios
```gherkin
Scenario: The behaviour that must hold
  Given ...
  When ...
  Then ...
```

## Notes
The traps. What looks right and is not.
````

Not every node needs every section — a grouping node is a sentence, and a node
tracking someone else's work has no deliverables of its own. But a node someone
will *claim and work* needs all of it. Write the acceptance criteria so that a
reviewer can check them without judgement calls.

## Gotchas

**Enum values are proto names.** `"priority": "PRIORITY_P2"`, not `"p2"`. Same
for `assignee_kind` (`ASSIGNEE_KIND_USER`), `status` (`STATUS_OPEN`), and drop
reasons (`DROP_REASON_SUPERSEDED`). The CLI's `--priority p2` takes the short
form; the patch API does not. `kind` is a free-form string (`task`, `bug`,
`spike`, `chore`), not an enum.

**`update_node` addresses its target with `node`, not `ref`.** So do
`drop_node`, `complete_node`, `reopen_node`, and `add_comment`. Only
`add_edge` differs: `from` and `to`.

**Edge direction is "must finish before".** `{"add_edge": {"from": "#3", "to":
"#7"}}` makes #7 wait for #3. Reversing it silently builds the wrong graph.

**A grouping node is not work.** If you give a node children, it leaves the
ready set until they all close. That is the point, but it surprises people who
add a child to a node someone is about to claim.

**A patch touching a leased node must carry that lease id.** A stale or expired
lease id is rejected, which is the fencing token doing its job — it means
someone else has the node now.

**Cycles are checked over the combined relation**, dependency edges *and*
parent containment together. A parent that ends up waiting on something that
waits on its own child is a cycle and the patch is rejected.

## Reference

| Node reference | Means |
|---|---|
| `#7` | node 7 of the current project |
| `slug#7` | node 7 of another project in this organization |
| `org/slug#7` | fully qualified |

The current project comes from `--project`, `$DG_PROJECT`, or the config file.
A bare `#7` with no current project is an error, so qualify it.

| Setting | Where |
|---|---|
| socket | `--socket`, `$DG_SOCKET`, then `/run/dagger/dg.sock` |
| remote service | `--address`, `$DG_ADDRESS` (wins over the socket) |
| config file | `--config`, `$DG_CONFIG`, then `$XDG_CONFIG_HOME/dagger/config.json` |

Priorities are `p0` (most urgent) to `p4`; `p2` is the default. Drop reasons are
`superseded`, `not_viable`, `no_longer_relevant`. Release reasons are `gate`
(you filed a gate and stepped back), `crash`, `budget`, `expired`, and
`preempted`; `complete` is recorded for you when you complete the node.

You are identified by the transport, not by anything you send: on a unix socket
by the uid on the other end, over a tailnet by the node's identity. There is no
field you can set to act as someone else. An uid that maps to no principal is
rejected as unauthenticated — if every call fails that way, the caller has no
principal, which is an administrative fix, not a client one.
