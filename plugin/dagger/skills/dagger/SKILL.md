---
name: dagger
description: Use when working with a dagger work graph — finding what is ready, claiming and completing work, decomposing work into linked nodes, or writing a node body another agent will pick up cold. Covers the dagger MCP tools and the dg CLI, when to prefer each, and the mistakes that silently build the wrong graph.
when_to_use: dg ready, dg claim, dg patch apply, apply_patch, show_node, inputs, explain, the ready set, claim a node, blocked_by, grouping node, work graph, backlog node, work order. This is the dagger DAG project manager reached through `dg`, not dagger.io the CI tool.
allowed-tools:
  - Bash(dg node show:*)
  - Bash(dg node list:*)
  - Bash(dg explain:*)
  - Bash(dg inputs:*)
  - Bash(dg search:*)
  - Bash(dg project list:*)
  - Bash(dg project show:*)
  - Bash(dg version:*)
  - mcp__plugin_dagger_dagger__ready
  - mcp__plugin_dagger_dagger__search
  - mcp__plugin_dagger_dagger__show_node
  - mcp__plugin_dagger_dagger__inputs
  - mcp__plugin_dagger_dagger__node_list
  - mcp__plugin_dagger_dagger__explain
  - mcp__plugin_dagger_dagger__project_list
  - mcp__plugin_dagger_dagger__project_show
---

# Dagger

**Autonomy:** model-invocable · reads the graph unprompted · claiming, patching and completing are the session's own calls, never pre-authorized here

Agent-first project management on a DAG. Dagger holds the plan; it does not run
agents, schedule work, or define workflows.

## The model

Ready = open, unblocked, unleased, **and no open children**. It is derived on
every query, never stored, so you change readiness only by changing its inputs.

| Relation | Means |
|---|---|
| edge / `blocked_by` | dependency — an edge from A to B makes **B wait for A** |
| `parent` | containment — a node with open children is a grouping node, never ready |
| lease | possession — claiming returns a lease id, the fencing token for every later write to that node |

A predecessor satisfies a dependency by being `done` **or** `dropped` — except a
**gate** (`assignee_kind: user`), satisfied only by a user completing it. A
dropped gate still blocks, so an agent cannot drop its own gate and proceed.

## The loop

`ready` → `show_node` → `claim` → **`inputs`** → work → `complete`.

`inputs` is the step that is easy to skip and expensive to skip. It returns the
results of the predecessors that satisfy the node — the handoff from whoever
did the work you depend on. `show_node` returns the node's own result and never
its predecessors'; what it carries is a hint, `inputs.count` and
`inputs.bytes`, so you know they are there and what reading them would cost.
`claim` carries the same hint. When the total is larger than you want at once,
ask for `view: "summary"` and read the ones you need with `show_node`.

A predecessor still open is not an input — it is what the node is waiting on,
which `explain` answers. A dropped one is listed with no result. Inputs are not
transitive: a predecessor's result is the summary of its own inputs.

## Which interface

MCP tools are the default: `ready`, `search`, `show_node`, `inputs`,
`node_list`, `explain`, `claim`, `heartbeat`, `release`, `complete`, `comment`,
`apply_patch`, `project_list`, `project_show`, `project_create`. A lease taken
through them renews in the background, and is released if the session ends.

`ready` is what you can work on; `node_list` is everything else — what is open,
what is done, what a label or a parent covers. Triage and reporting go through
`node_list`, and it never tells you what is ready.

`project_list` is how you find a slug when no current project is configured, or
when you need to write a cross-project `slug#7`; `project_show` reads one
project, `project_create` makes one. None of them lists work.

Responses are sized for a context window. A write returns references, status
and readiness flags for what it touched, an alias map, and an event count —
never the bodies you sent, never the events. `ready`, `search` and `node_list`
return summaries (reference, title, kind, priority, status, labels); only
`show_node` returns a body. Every one of these takes `view: refs | summary |
full` when you need more or less, so filing a large patch through MCP costs
about the same as filing a small one.

Reach for the `dg` CLI for shell pipelines, hooks, and `dg ready --wait`.

Enum values are short and lowercase on every surface, in JSON as in the CLI
flags: `"priority": "p2"`, `"assignee_kind": "user"`, `"reason": "superseded"`.
The proto spellings (`PRIORITY_P2`) are accepted too. `kind` is not an enum: it
is a free string, conventionally `task`, `bug`, `spike`, or `chore`.

## Traps

- **`update_node` names its target `node`, not `ref`** — as do `drop_node`,
  `complete_node`, `reopen_node`, `add_comment`. Only `add_edge` differs
  (`from`/`to`).
- **Edge direction is "must finish before".** Reverse it and you get a valid
  graph that is the wrong one; nothing errors.
- **Giving a node a child takes it out of the ready set** until every child closes.
- **`dg ready --wait` claims** — the CLI verb is overloaded, which is why
  `Bash(dg ready:*)` is not pre-authorized above. The MCP `ready` tool takes no
  such flag and never claims.
- **A cascading drop takes the whole blocked subtree** in one transaction. Read
  the graph before setting `cascade`.
- **Never claim a node whose `assignee_kind` is `user`.** That is a gate — a
  decision a human owes, deliberately blocking something.

## References

- [references/commands.md](references/commands.md) — every command with worked
  examples, the patch operations and aliases, node references, socket and
  address settings, how identity is resolved
- [references/work-orders.md](references/work-orders.md) — what a node body owes
  an agent starting with an empty context window, and the template
