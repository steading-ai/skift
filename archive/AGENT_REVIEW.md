# Agent review

Notes from driving skift as a client rather than building it: interface
friction, and where the token cost falls. Every number here was measured
against this repository and a live service on 2026-09-06; every claim about
behaviour is cited to a file.

The user's feedback and comments are between `%%` markers.

## How this was produced

One agent session used skift for real work: created a project, filed 28 nodes
in four patches, then rewrote all 28 bodies in a single 28-operation patch, and
read the graph back throughout. The findings below are the places that cost
something — a failed call, a wrong graph, or context window.

Two things went wrong on first attempt. Both were interface defects rather than
mistakes an agent could reasonably avoid, and both are fixable at the source.

---

## 1. Responses echo the request back, at 20–215×

The largest single cost, and the one that most limits skift in a harness where
MCP is the only door.

Measured, on the 28-operation patch that rewrote every node body:

| | bytes | ~tokens |
|---|---|---|
| `apply_patch` response | 185,588 | 46,000 |
| — of which node bodies | 57,486 | the caller had just sent these |
| — of which `event.after` | 72,206 | the same node state again, once per operation |
| what a caller can act on (`ref` + derived flags) | 865 | |

**215× overhead.** The response is the request, twice, plus an audit log.

`ready` has the same shape at smaller scale: 22 ready nodes returned 54,139
bytes (~13,500 tokens) where number, title, kind and priority — everything
needed to choose what to work on — is 2,738. **20×.**

For context: filing that one patch through MCP would consume roughly a quarter
of a 200k context window in echo alone, before any work is done.

**The workaround makes it worse.** The skill written for this plugin advises
using the CLI for bulk writes, because `skift patch apply - < patch.json` returns
the same result where it can be summarised by a pipeline instead of landing in
the conversation. That is real advice today, and it is unavailable in a
harness that has only MCP — the environments skift most wants to serve.

**Suggestion.** A `view` parameter on the read and write paths:

- `refs` — the alias→reference map, each node's number, and the derived
  `ready`/`blocked` flags. The default for `apply_patch`.
- `summary` — the above plus title, kind, priority, status. The default for
  `ready`, `node list`, and `search`.
- `full` — today's behaviour, on request.

Two rules would carry most of the win on their own:

1. **Never echo `body` on a write.** The caller sent it; returning it teaches
   nothing and costs the most.
2. **Do not return `events` by default.** Events are the audit record and are
   queryable. A patch response can carry `event_count` and the request id.

`show_node` is the one place `full` should stay the default — it is the read
whose whole purpose is the body.

## 2. `apply_patch` is unschema'd, and its description misdescribes it

The tool's input schema for the thing that does all the work is:

```json
"ops": {"type": "array", "items": {"type": "object"}}
```

A model gets no structural guidance at all — every operation's shape is guessed
from prose. Both first-attempt failures in the session trace here.

The prose is also wrong. `internal/cli/mcp_tools.go:219-221` names nine
operations; compared against the `PatchOp` oneof in
`proto/skift/v1/service.proto:122-146`:

- **Named but do not exist:** `drop_edge`, `reparent_node`, `set_priority`
- **Exist but unnamed:** `update_node`, `attach_artifact`, `edit_comment`,
  `resolve_comment`

`update_node` is the operation an agent revising a backlog needs most, and it is
the one the description omits. Having no schema and no mention, the session
guessed its target field was `ref`; it is `node`
(`proto/skift/v1/service.proto:185`).

The three named-but-absent operations are a naming question rather than missing
features — all eleven `PatchOp` variants are implemented and dispatched. §6
works through what they map to, and what checking it turned up.

**Suggestion.** Generate the `ops` JSON Schema from the proto — a `oneOf` over
the eleven operation messages, with each one's fields and enum values. It
cannot drift from the contract because it is derived from it, the phantom
operations disappear, and a model can no longer guess a field name because the
schema names them. This subsumes most of §3 and §9 as well.

If generating the full schema is too large a change to make at once, listing
the eleven operations correctly is a five-minute fix worth doing immediately.

## 3. Enum values on the wire are long, and inconsistent with every other surface

`"priority": "PRIORITY_P2"` where the same value is `p2` in the CLI flag, `p2`
in the display, and `p2` in the docs. The patch API is the only surface using
the proto-JSON form, which is what makes it a trap rather than merely verbose:
an agent that has read the CLI help and the domain model has been taught the
short form everywhere it looked.

The byte cost is real but secondary — eleven characters against two, times
every operation. The learnability cost is the expensive one.

**Do not fix this field by field.** The same asymmetry exists for `Status`,
`AssigneeKind`, `EdgeKind`, `DropReason` and `ReleaseReason`, and will exist for
the next enum added.

**Suggestion.** One codec at the proto-JSON boundary, driven by protoreflect:

- **Accepting:** for any enum field, accept the canonical name
  (`PRIORITY_P2`), the value name minus the enum's own prefix (`P2`), and that
  lowercased (`p2`). The prefix is derivable from the enum descriptor, so this
  needs no per-enum table and covers enums that do not exist yet.
- **Emitting:** emit the short form.

The proto definitions stay exactly as they are — prefixed values are correct for
generated-code scoping and should not change to serve a JSON encoding.

Where to put it is a real decision. Doing it in the Connect codec changes the
wire contract for every client; doing it in the MCP and CLI layers only leaves
the wire canonical and fixes it for the callers who actually suffer. The second
is smaller and reversible, and is probably right for now.

%%
It's fine, there's no other clients at this point. Let's do this right.
%%

Independently: **declare the legal values in the MCP tool schemas**
(`"enum": ["p0","p1","p2","p3","p4"]`). A model that can see the values in the
tool definition does not need to be told them, and cannot get them wrong.

## 4. Tool descriptions are the right length; they are missing the *why*

Measured: ten tool descriptions total 3,860 characters, about 965 tokens, plus
552 characters of server instructions. That is not a token problem — it is a
reasonable always-on cost for a ten-tool server, and cutting it would be
optimising the wrong thing.

What is missing is intent. `p0`–`p4` is a project-management idiom, and the
descriptions never say what the scale *means*: that it is advisory ordering
within the ready set rather than a gate, that `p2` is the default, that
inflation makes it useless because a backlog where everything is `p1` orders by
age. An agent that does not know this either avoids priority entirely or marks
everything urgent.

The best description in the set already does this. `ready` says **"Nothing is
claimed. Use it to see what could be worked on next; use claim to take one."**
— it states what the tool does *not* do and points at the tool that does. That
is the pattern worth propagating.

**Suggestion.** A description should answer four things, and most currently
answer one:

1. What it does.
2. What it does **not** do, where a reasonable reader would assume otherwise.
3. What the values mean — the idiom behind them, not just the range.
4. What happens when you get it wrong, if the failure is quiet.

Candidates where this is missing today: priority (the idiom), `kind` (free
string, conventionally `task`/`bug`/`spike`/`chore` — an agent cannot know the
convention from the schema), and `assignee_kind` (see §8).

## 5. MCP cannot see projects at all

The MCP server registers ten tools: `apply_patch`, `claim`, `comment`,
`complete`, `explain`, `heartbeat`, `ready`, `release`, `search`, `show_node`.
There is no project tool of any kind.

So an MCP-only agent cannot list the projects it may read, cannot create one,
cannot read a project's title or priority, and cannot discover the slug it needs
to write a cross-project `slug#7` reference. The session hit this immediately
and used the CLI, which an MCP-only harness cannot do.

This compounds §1: the two things that force an agent out to the CLI are
response size and project discovery, and both are exactly what an MCP-only
environment cannot work around.

**Suggestion.** Add `project_list`, `project_show`, and `project_create`.
Archive is rarer and can wait. If ten tools is felt to be the budget, note that
a `view` parameter (§1) buys back far more context than three small tools cost.

## 6. Built, but with no door

Checking whether the operations §2 calls absent were *wanted* turned up
something more interesting: two capabilities that are finished in the domain
and unreachable by any client.

### The named-but-absent operations are all reachable

| Description says | Actually | Verdict |
|---|---|---|
| `drop_edge` | `remove_edge` | renamed |
| `reparent_node` | `update_node.parent` (`service.proto:186-188`) | folded in |
| `set_priority` | `update_node.priority` (`service.proto:195-196`) | folded in |

All eleven `PatchOp` variants are implemented and dispatched
(`internal/domain/patch.go:721-743`). Nothing hides behind those three names,
and they are not worth adding as operations.

They may be worth adding as **CLI verbs**. `create`, `link`, `comment` and
`complete` each have a shortcut that builds the patch for you; reparenting,
repriorotising, editing a body and dropping all require hand-writing one. That
asymmetry is probably why the description reached for names that sound like
verbs — the shortcuts are the thing that is missing, not the operations.

%%
Let's scope this in too.
%%

### Two capabilities are implemented and have no contract

**Predecessor results — the handoff mechanism.** `internal/domain/inputs.go`
implements `Inputs`: the results and artifacts of every node blocking a given
node, which is precisely what a downstream agent assembles its context from. It
is exercised by specs and has **zero non-test callers**. There is no RPC, no
MCP tool, no CLI command; `proto/` mentions inputs only in a comment
(`service.proto:228`).

This is the capability that makes a graph of agent work more than a task list —
backlog #22's scenario is "a design agent hands work to a coding agent" — and
no client can ask for it today. `show_node` returns a node's own result, never
its predecessors'.

%%
We should give this some thought since token efficiency is a concern. Maybe a separate tool + hints to retrieve downstream inputs?
%%

**Usage and estimates.** `internal/domain/usage.go` implements `ReportUsage`
(line 200) and `OpEstimateNode`, both authorized (`authz.go:204`) and dispatched
(`patch.go:744-745`). Neither appears anywhere in `proto/`. An agent cannot
report what an attempt cost or record an estimate, though the domain is waiting
to receive both.

%%
An agent can estimate but the orchestrator should be responsible for reporting.
%%

Backlog #22, #25 and #26 are open, so this is work in flight rather than
neglect. It is worth flagging for one reason that generalises beyond these two:

> An acceptance criterion satisfied in `internal/domain` is not satisfied for a
> client. #22's "Predecessor results are retrievable as the inputs to a node"
> is true of the domain and false of every interface. A criterion that does not
> name the surface it holds at can be honestly checked off while the capability
> stays unreachable.

### Missing from MCP, present in the contract

Beyond the project tools of §5, MCP omits two more of the fourteen RPCs:

| RPC | CLI | MCP |
|---|---|---|
| `ListNodes` | `skift node list` | — |
| `CreateProject` / `GetProject` / `ListProjects` / `ArchiveProject` | `skift project …` | — |
| `WatchEvents` | `skift events` | — |

`ListNodes` is the sharpest. Without it an MCP-only agent can see the ready set
and run a text search, and nothing else — it cannot ask what is blocked, what is
in flight, what it finished, or what carries a label. `explain` answers only for
a node it can already name. Triage and reporting are both out of reach.

## 7. Cascading drops: the danger is breadth, not permanence

Worth correcting the framing first, because it changes what guardrail is right.
**A drop is reversible.** `reopenNode` rejects only a node that is already open
(`internal/domain/patch.go:935-941`), so a dropped node can be reopened, and
edges are untouched by a drop. A cascade is recoverable.

What makes it dangerous is that it is **wide, silent, and hard to enumerate
afterwards**. `cascade: true` drops the whole blocked subtree in one
transaction. The caller may not know how large that subtree is, nothing makes
them look, and undoing it requires knowing which nodes were dropped — which is
only in the response they may not have kept.

Telling an agent to be careful is not a control; that instruction competes with
everything else in its context, and this is precisely the case where a
structural gate beats prose.

**Suggestions, best first.**

1. **Make the caller state the blast radius.** `cascade: {max_nodes: N}` — the
   server counts the subtree and refuses if it exceeds `N`, naming the real
   count. A caller who has not looked cannot supply a truthful `N`, and one who
   has loses nothing. No extra round trip, works for every client, and needs no
   policy engine.
2. **Always return the dropped set** (refs only — cheap even under §1's `refs`
   view), so the undo is mechanical rather than archaeological.
3. **A policy veto for the large case.** Skift already evaluates Rego in
   process. A rule refusing a cascade above some size to a non-`user` principal
   is a gate that holds regardless of what the model was told, in the same
   place the other vetoes live (`policy/skift.rego:226-241`).

%%
Policy veto could make sense. I don't think we need it *now* though.
%%

4. `dry_run: true` returning the set without dropping is the most flexible and
   the most work; (1) gets most of the benefit for much less.

One property already correct and worth protecting: a dropped gate still blocks
(`internal/domain/ready.go:65-76`), so a cascade cannot be used to clear a gate
out of the way.

## 8. "Never claim a user-assigned node" is convention, and probably wrong advice

This one deserves rethinking rather than documenting.

**What is actually enforced.** `policy/skift.rego:228-233` vetoes
`complete_node` on a gate by any non-`user` principal, and `:237-241` vetoes the
principal that *filed* a gate from disposing of it at all. Both are about
**completion**. Nothing prevents an agent from claiming a gate — the "never
claim" rule is a convention from this repository's own backlog header, not a
property of the system.

**Why the convention is too strong.** Consider a gate that says "decide the
subscription pricing". An agent cannot decide it — correctly. But it can evaluate options, research competitors,
comment its findings on the node, make a recommendation,
and release with reason
`gate`. The human then arrives at a decision that is already researched. That
is strictly better than the agent skipping the node, and it is what the lease
and the `gate` release reason appear designed for.

%%
I edited *convention is too string*. Truthfully, I don't agree with the example. It's situational and depends on the project. A clearer example is for flagging when a human is *required*. For example, if I have to sign up for a service, mint a token through an authenticated web UI, or pull out my credit card. I'd prefer to loosen the default policy quite a bit and use the human owner as a notification mechanism for me to unblock agents, not a safety requirement. Other orgs may have different needs.
%%

So the useful rule is not *never touch* but **claim to prepare, never to
decide**, and the guidance should say so.

%%
I'm not sure about this either. I'd think that "a human needs to XYZ" is a node that depends on "an agent prepares a recommendation" and any other prerequisites. This may be an agent misunderstanding -- maybe docs are needed.
%%

**The harder question — an agent acting on a human's behalf.** The proto already
has `on_behalf_of`, deliberately not honoured in this release, and
`docs/adrs/006-provenance-not-authority.md` says why: a field a client asserts
is not provenance. That is the correct constraint, and it means delegation
cannot be a flag.

What it can be is a **grant**: a `user` principal records a delegation to an
agent principal — scoped to a project, expiring, revocable, stored server-side.
Policy then permits gate completion when a live grant covers the caller, and the
event records both principals, so the audit answers "who did this" and "on whose
authority" separately. Nothing is client-assertable, and the interactive case
the question comes from — a human working directly with an agent — is served
without weakening the gate for unattended loops, which is the case the gate
exists for.

%%
I'd punt on grants. Maybe somewhere on the roadmap in the future if/when we actually want stronger policy.
%%

That is a real feature and should be a node, not a patch. Two things are worth
doing before it:

- Change the guidance from "never claim" to "claim to prepare, never to
  decide"; document that an agent may comment and release with reason `gate`.
- Make the veto's message say *why* and what to do instead. An error a model
  reads is an opportunity to teach it the right move; right now it only says no.

## 9. Errors do not carry the fix

Both first-attempt failures in the session produced errors that named the
problem and withheld the answer, though the server knows it:

```
invalid value for enum field priority: "p2"        # legal values not named
unknown field "ref"                                 # legal fields not named
```

These are raw proto unmarshal errors surfaced to the caller. In an agent loop,
a tool error is not a log line — it is the next prompt, and it will be acted on
immediately. An error that names the legal values costs one retry; one that does
not can cost several, or a silent fallback to a worse approach.

**Suggestion.** Wrap unmarshal failures at the MCP and CLI boundary: on an enum
failure, list the accepted values; on an unknown field, list the fields of the
message that failed. Both are available from the descriptor.

%%
100% agree.
%%

## 10. Smaller things

- **`ready` means two things.** The CLI's `skift ready --wait` blocks and returns
  the node *claimed under a lease* (`internal/cli/work.go:82-90`), while the MCP
  `ready` tool has no `wait` parameter and never claims. One name, two
  behaviours, split across interfaces. Consider making `--wait` on the CLI an
  alias for `claim --wait` and removing it from `ready`, so the verb means one
  thing everywhere.
- **`drop` has no CLI verb.** `create`, `link`, `comment` and `complete` all have
  shortcuts that build a patch; dropping requires hand-writing one. Given §7,
  a `skift drop` that requires an explicit blast radius would be a good place to
  put the guardrail.
- **`skift events` cannot answer "what happened"** — it streams until interrupted,
  so a scripted verification hangs. Already filed as issue #5.
- **`kind` is a free string with a convention no interface states.**
  `task`/`bug`/`spike`/`chore` appear in the CLI help as examples; nothing says
  whether they are canonical. Either constrain it or say plainly that it is
  free-form and these are conventional.

---

## What I would do first

Ordered by benefit per unit of work.

1. **§2, listing the eleven operations correctly.** Minutes; removes a defect
   that actively misleads, and the only one on this list that is purely a
   mistake rather than a trade-off.
2. **§1, `view` with `refs` as the write default and no `body` echo.** The
   single largest token win in the system, and the thing standing between
   skift and an MCP-only harness.
3. **§6, an RPC and tool for `Inputs`.** The domain is written and tested; what
   is missing is a door. Until there is one, an agent cannot read what the work
   it depends on produced, which is the premise the rest of the design rests
   on. Probably the highest value per line of new code anywhere here.
4. **§9, errors that name legal values.** Small, and every one saved is a retry
   an agent does not spend.
5. **§3, the enum codec, in the MCP and CLI layers.** One generic change that
   closes the trap for every enum that exists or will.
6. **§5 and §6, MCP coverage: `node_list` and the project tools.** Four small
   tools that between them remove every remaining reason to shell out to the
   CLI. `node_list` is the one an agent misses first.
7. **§2 properly, the generated `ops` schema.** Larger, and it subsumes part of
   §3 and §9 — worth doing once the shape of those is settled.
8. **§7, `cascade: {max_nodes: N}`.** A real control where there is currently a
   hint.
9. **§6, a contract for usage and estimates.** The domain is ready; this is
   finishing #26 rather than starting it.
10. **§8, delegation grants.** The largest, and the one that wants a design
    discussion before it wants a node.

Items 1, 4 and 6 are each an afternoon at most and would change the experience
of using skift through MCP more than anything else on the list.

## A note on what this review is not

These are the observations of one agent that used the system hard for a day, on
one project, over a unix socket. It did not exercise artifacts, comments at
volume, `search` beyond a few queries, the tailnet transport, concurrency, or
anything at scale. Where a finding rests on a measurement, the measurement is
reproducible from this repository. Where it rests on judgement — §8 especially —
it is an opinion offered for argument, not a defect report.
