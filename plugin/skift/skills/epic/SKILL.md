---
name: epic
description: Use when work is large enough to plan before filing — writing a mini-PRD for an epic, decomposing one into skift nodes as one Markdown file each, or reviewing a draft decomposition before it is filed. Covers the epic document, the patches directory, the YAML frontmatter a node file carries, and how a directory becomes one patch. Trigger phrases — write this up as an epic, draft an epic, mini-PRD, decompose this into nodes, plan this before filing, epic document, work orders as files, node frontmatter, patches directory, file a decomposition, skift patch import, skift node export.
allowed-tools: Read Write Edit Glob Grep Bash(skift ready:*) Bash(skift node show:*) Bash(skift node list:*) Bash(skift explain:*) Bash(skift search:*) Bash(skift project list:*) Bash(skift project show:*) mcp__plugin_skift_skift__ready mcp__plugin_skift_skift__search mcp__plugin_skift_skift__show_node mcp__plugin_skift_skift__node_list mcp__plugin_skift_skift__explain mcp__plugin_skift_skift__project_list mcp__plugin_skift_skift__project_show
---

# Epic

**Autonomy:** model-invocable · drafts the document and the node files · applying a patch is the person's act and is never pre-authorized here

An epic is **two artifacts that travel together**, kept with the project's notes
rather than in the repository:

```
<epic-slug>.md                 the mini-PRD: what is wrong, what changes, what is out of scope
<epic-slug>/patches/
  README.md                    the shape, the sizing, what to do before applying
  <project-slug>/              one directory per skift project
    <alias>.md                 one node per file — frontmatter is the op, body is the work order
```

**Agents cannot read the document.** They receive a node body and nothing else.
So every body restates what its reader needs, and the document is never named as
an agent's authority — only files in the repository are.

## When to draft one

More than about five nodes, or any node whose body runs longer than a paragraph,
or a design decision that has to be settled before code. Below that, file an
inline patch with `apply_patch` and skip all of this.

## Sequence

1. **Settle the design before decomposing.** Read what governs it and say what
   is actually wrong, with evidence — paths, commands, counts. A decomposition
   of an unsettled design is a set of nodes that argue with each other.
2. **Write the document** — [references/epic-doc.md](references/epic-doc.md) has
   the sections, what each is for, and the bar each has to clear.
3. **If the epic decides anything, the first node writes that decision into the
   repository** — an ADR, a document under `docs/` — and every node after it
   cites the file, not the epic. This is what makes the rest of the graph
   workable by an agent with an empty context window.
4. **Decompose into one file per claimable unit** — [references/node-files.md](references/node-files.md) has the layout, the frontmatter keys, and how references resolve. The body follows the `skift` skill's work-order reference, [../skift/references/work-orders.md](../skift/references/work-orders.md): Context, Deliverables, Acceptance criteria, Scenarios, Notes.
5. **Write the patches README** — the dependency shape as a diagram, the sizing,
   what must be true before applying, and what the epic proves when it lands.
6. **Proofread against the graph.** `skift patch import <dir> --dry-run` prints the
   patch without writing anything; `skift node export` renders what is already
   filed in the same dialect, so a draft can be diffed against reality. Neither
   has shipped yet: until then, assemble the directory into a patch and
   `skift patch apply` it, and read the store with `skift node show` — see
   [references/node-files.md](references/node-files.md).
7. **Stop there.** Say what you would apply and let the person apply it.

## Hard rules

- **One node per separately claimable piece of work.** Giving a node children
  takes it out of the ready set until they all close — that is what makes a
  grouping node a milestone rather than a task.
- **`blocked_by` is dependency, never ordering preference.** An edge that only
  encodes a preference serialises work that could have run in parallel, and the
  cost is invisible until someone asks why one node is ever ready.
- **A gate is work only a person can do** (`assignee_kind: user`). The agent work
  that makes a gate easy is a *separate node the gate depends on*, never the
  gate's own body.
- **Size against the calibration that held:** under eight acceptance criteria and
  six scenarios per node, one package or cluster each. A node past that is two
  nodes.
- **Name the files a node may not touch** in its Notes when the repository
  protects any, and never promise a capability the epic defers.

## References

- [references/epic-doc.md](references/epic-doc.md) — the mini-PRD: every section,
  its purpose, its bar, and the skeleton
- [references/node-files.md](references/node-files.md) — the file format: layout,
  the frontmatter keys, references, filing with `skift patch import`, reading back
  with `skift node export`, and the traps
- [../skift/references/work-orders.md](../skift/references/work-orders.md) — from the `skift` skill, installed beside this one: what a node body owes an agent starting cold
- [../skift/SKILL.md](../skift/SKILL.md) — the `skift` skill itself: the graph model of readiness, holds, leases and gates, and which interface to reach for
