# `skift:skift` — Driving the Work Graph

Teaches an agent to take, do and finish work in a skift graph, through the
MCP tools or the `skift` CLI. Model-invoked whenever a conversation touches
a graph, and invocable by name.

```
/skift:skift
/skift:skift claim the next node and work it
/skift:skift decompose this into nodes an agent can pick up cold
```

## Why this exists

Skift's surface reads like a search engine and is not one. An agent that
arrives wanting work reaches for the verbs it knows — list, search, pick the
most promising node, take that one — and every step of that instinct is wrong
here:

- **`claim` names no node.** It *is* the selector. Nothing accepts the node you
  picked out of a listing, so searching first produces a choice the service
  will not honour.
- **`claim` already returns the body.** A `show_node` before it fetches a
  summary of what the claim was about to hand over whole.
- **A listing is stale as it prints.** Claims lock `SKIP LOCKED`, so the node an
  agent is reading about is the node a concurrent claimant just took.
- **The ready set contains gates.** A claim that does not filter for agent work
  can land on work only a person may perform, and policy refuses it outright
  rather than moving on to the next candidate.

So the body opens on `claim` → `inputs` → work → `complete`, says plainly that
nothing precedes the claim, and demotes reading the graph to what it is
genuinely good for: explaining an empty answer, triage, and reporting.

The second failure is quieter. Three relations in the model all read as
"depends on" — an edge, a parent, a hold — and choosing wrong builds a graph
that is valid, silent, and not the one anyone meant. `Traps` is that list, kept
short enough to survive in a context window.

## Files

| File | Holds |
|---|---|
| `SKILL.md` | the loop, the model, the traps — what an agent needs in context |
| `references/commands.md` | every command, the patch operations, settings, identity |
| `references/work-orders.md` | what a node body owes an agent starting cold |

Installation, the MCP registration, and the version-as-cache-key gotcha are in
[the plugin README](../../README.md).
