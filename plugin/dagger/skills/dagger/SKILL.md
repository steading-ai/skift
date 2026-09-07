---
name: dagger
description: Use when working with a dagger work graph — finding what is ready, claiming and completing work, decomposing work into linked nodes, or writing a node body another agent will pick up cold. Covers the dagger MCP tools and the dg CLI, when to prefer each, and the mistakes that silently build the wrong graph.
when_to_use: dg ready, dg claim, dg patch apply, apply_patch, show_node, explain, the ready set, claim a node, blocked_by, grouping node, work graph, backlog node, work order. This is the dagger DAG project manager reached through `dg`, not dagger.io the CI tool.
allowed-tools:
  - Bash(dg node show:*)
  - Bash(dg node list:*)
  - Bash(dg explain:*)
  - Bash(dg search:*)
  - Bash(dg project list:*)
  - Bash(dg project show:*)
  - Bash(dg version:*)
  - mcp__plugin_dagger_dagger__ready
  - mcp__plugin_dagger_dagger__search
  - mcp__plugin_dagger_dagger__show_node
  - mcp__plugin_dagger_dagger__explain
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

## Which interface

MCP tools are the default: `ready`, `search`, `show_node`, `explain`, `claim`,
`heartbeat`, `release`, `complete`, `comment`, `apply_patch`. A lease taken
through them renews in the background, and is released if the session ends.

Responses are sized for a context window. A write returns references, status
and readiness flags for what it touched, an alias map, and an event count —
never the bodies you sent, never the events. `ready`, `search` and `node list`
return summaries (reference, title, kind, priority, status, labels); only
`show_node` returns a body. Every one of these takes `view: refs | summary |
full` when you need more or less, so filing a large patch through MCP costs
about the same as filing a small one.

Reach for the `dg` CLI for shell pipelines, hooks, and `dg ready --wait`.

## Traps

- **Patch enums are proto names**: `"PRIORITY_P2"`, not `"p2"`; likewise
  `ASSIGNEE_KIND_USER`, `DROP_REASON_SUPERSEDED`. The CLI's `--priority p2`
  takes the short form, the patch API does not. `kind` is a free string.
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
