# Commands

Every example is the `skift` CLI. Each MCP tool is the same command with the same arguments and the same JSON response, so translating either way is mechanical.

Output is JSON whenever stdout is not a terminal, and on request with `--output json`. Errors are a JSON object on stderr: `{"code":…, "message":…}`, and the exit code is non-zero.

## MCP or the CLI

The MCP tools are `ready`, `search`, `show_node`, `inputs`, `node_list`, `explain`, `claim`, `heartbeat`, `release`, `complete`, `comment`, `apply_patch`, `hold`, `focus`, `project_list`, `project_show`, `project_create`. The bundled `skift mcp` **stdio** process renews leases it claimed in the background and attempts bounded release on stdin EOF or handled signal; hard kill or failed cleanup leaves expiry. **Native HTTP** requires explicit heartbeat before expiry, explicit `lease_id` on holder writes, and complete/release when done. HTTP EOF, disconnect and service shutdown do not release committed claims. Successful holder writes may also renew under the existing domain rules; reads do not. CLI commands also require explicit lease care. Expired or stale fencing tokens cannot write after reclaim.

Responses are sized for a context window. A write returns references, status and readiness flags for what it touched, an alias map, and an event count — never the bodies you sent, never the events. `ready`, `search`, `show_node`, `inputs`, `node_list`, `apply_patch`, `complete` and `comment` each take `view: refs | summary | full` when you need more or less, so filing a large patch through MCP costs about the same as filing a small one. `claim` takes no view: it answers with the whole node either way.

Enum values are short and lowercase on every surface, in JSON as in the CLI flags: `"priority": "p2"`, `"assignee_kind": "user"`, `"reason": "superseded"`. The proto spellings (`PRIORITY_P2`) are accepted too. `kind` is not an enum: it is a free string, conventionally `task`, `bug`, `spike`, or `chore`.

## Finding and reading

```sh
skift ready                          # what can be worked now, most urgent then oldest; claims nothing
skift ready --assignee-kind user     # the gates
skift ready --kind bug --min-priority p1 --label backend
skift node show '#7'                 # one node: body, edges, result, lease, artifacts, comments
skift inputs '#7'                    # the results of the predecessors #7 depends on
skift node list                      # the project's nodes, filtered
skift explain '#7'                   # why it is not ready; no reasons means it is
skift search 'socket "permission denied" -legacy'
skift project list                   # most recently active first
skift project show platform          # one project, its repository included
skift project create Platform --vcs-url https://gitlab.example/x/platform.git
skift project update platform --vcs-url ''   # clear the repository; unset flags leave fields alone
```

`ready`, `node list` and `search` return a **summary** of each node: its reference (`slug#7`), title, kind, priority, status, labels, and the derived `ready`/`blocked`/`claimed` flags. `node show` returns everything, body included, and so does `claim` for the node it takes — which is why claiming needs no `node show` after it. `--view refs|summary|full` (the `view` argument of the MCP tools) changes that: `--view full` on a listing carries every body, `--view refs` is references and flags alone.

`search` is PostgreSQL websearch syntax: bare words, `"quoted phrases"`, `-excluded`, and `OR`. It covers node titles, bodies, results and comments across every project you can read unless `--project` narrows it, and skips archived projects unless `--include-archived`.

`inputs` is the handoff: one entry per predecessor that satisfies the node, with the result it completed with and the artifacts it attached. `node show` never returns it — a node's own result is not its predecessors' — and instead reports `inputs.count` and `inputs.bytes`, the same hint `claim` carries, so you can decide whether to fetch. `--view summary` gives references, titles and per-entry sizes with no bodies, for reading a large handoff one predecessor at a time. An open predecessor is not an input; a dropped one is listed with no result; inputs are not transitive.

Comments come the same way. `claim` carries `comments.unresolved` and `comments.latest_at` — how many comments on the node are unresolved, and when the newest was written — and `node show` carries the same hint as `comments_hint` under every view, with the comments themselves under `--view full` alone. A reply is resolved on its own, so it counts under a resolved parent. An edit does not move `latest_at`.

`explain` returns structured reasons — blocking nodes, a live lease and its expiry, waiting on a user — not prose. Prefer it over inferring from `show`.

## Taking and finishing

```sh
skift claim --assignee-kind agent               # the node itself, body and all, AND a lease id
skift claim --assignee-kind agent --min-priority p2 --kind task
skift claim --assignee-kind agent --wait 30m    # block until something matches
skift claim --assignee-kind agent --wait        # block indefinitely
skift heartbeat "$LEASE"                        # renew; any write by the holder also renews
skift complete '#7' --lease "$LEASE" --result 'What was produced, and where.'
skift complete '#7' --lease "$LEASE" --result - # result body from stdin
skift release "$LEASE" --reason gate --detail 'needs a decision on the schema'
```

The lease id is a fencing token: a write carrying a stale or expired one is rejected, which is how a reclaimed node resists its previous holder.

The **result body is the point of finishing**. Downstream agents assemble their context from the results of what blocked them, so say what was produced and where it lives. A result of "done" throws that away.

Release reasons: `gate` (you filed a gate and stepped back), `crash`, `budget`, `expired`, `preempted`. `complete` is recorded for you when you complete.

## Changing the graph

`apply_patch` is the only way to mutate. A patch is a list of operations applied in one transaction and cycle-checked as a whole: if one operation is rejected, nothing in the patch happened.

Operations: `create_node`, `update_node`, `drop_node`, `complete_node`, `reopen_node`, `add_edge`, `remove_edge`, `attach_artifact`, `add_comment`, `edit_comment`, `resolve_comment`, `estimate_node`, `hold_node`, `unhold_node`.

**Aliases make one patch a whole subgraph.** A `create_node` declares an `alias`; any later operation in the same patch refers to the new node by it, with no round trip to learn its number:

```json
{"ops": [
  {"create_node": {"alias": "spike", "title": "Spike the parser", "kind": "spike",
                   "priority": "p1"}},
  {"create_node": {"alias": "impl", "title": "Implement it", "blocked_by": ["spike"]}},
  {"create_node": {"alias": "docs", "title": "Document it", "parent": "impl"}},
  {"add_edge":    {"from": "impl", "to": "docs"}}
]}
```

The response is what the patch did, not what it was sent: every node it created or changed as a reference with its number, status and readiness flags, the map from each alias to the reference it became, `event_count`, and the `request_id` it was applied under. Bodies are never echoed and events are not returned; `--view full` (or `view: "full"`) returns every field and the events, and `skift events` reads them from the log at any time.

```sh
skift patch apply patch.json
skift patch apply -                  # from stdin
skift patch apply - --view full      # everything back, events included
```

Field names differ per operation. Most name their target `node`; `add_edge` and `remove_edge` use `from` and `to`. Reparenting and reprioritising are `update_node` fields (`parent`, `priority`):

```json
{"update_node":   {"node": "#7", "body": "…", "priority": "p0"}}
{"drop_node":     {"node": "#7", "reason": "superseded",
                   "detail": "folded into #9"}}
{"drop_node":     {"node": "#7", "reason": "not_viable",
                   "cascade": {"max_nodes": 5}}}
{"complete_node": {"node": "#7", "result": {"body": "…"}}}
{"add_comment":   {"node": "#7", "body": "…", "in_reply_to": "<comment id>"}}
{"estimate_node": {"node": "#7", "model": "claude-opus-5", "basis": "…",
                   "input_tokens": 120000, "output_tokens": 8000}}
```

A patch touching a node you hold a lease on must carry that `lease_id`.

For one small change the CLI builds the patch for you; each answers in the same refs view, and takes the same `--view`:

```sh
skift create 'Title' --body - --kind bug --priority p1 --blocked-by '#3' --label api
skift create 'Decide the retention policy' --assignee-kind user   # files a gate
skift update '#7' --priority p0 --label api --label backend       # replaces the labels
skift update '#7' --body - --title 'Retitled'                     # body from stdin
skift update '#7' --parent '#3'                                   # reparent; --parent '' roots it
skift link '#3' '#7'                                              # #3 before #7
skift drop '#7' --reason superseded --detail 'folded into #9'
skift drop '#7' --reason not_viable --cascade 5   # and the subtree, up to 5 nodes
skift comment '#7' 'note' --in-reply-to '<id>'
skift estimate '#7' --input 120000 --output 8000 --basis 'two nodes of the same shape'
```

`skift update` sends only the flags you pass, so everything else is left alone. An empty value is a value: `--parent ''` makes the node a root, `--assignee ''` unassigns, and `--label ''` clears the label set. Status is not among its flags — it changes through `complete`, `drop` and `reopen` alone.

`skift drop` answers with `dropped`, the reference of every node it took: the list a `reopen_node` undoing the drop works from. `--cascade` takes the most nodes the cascade may drop, counting the node named; the service refuses the patch and names the real count when the subtree is larger.

Every write takes `--request-id` (MCP `request_id`) as an idempotency key. Persist a unique key before sending a mutation. Retrying the same arguments with the same key returns the original response and writes no second set of events; different content with that key conflicts. JSON-RPC `id` is not this key. Omitting it mints a fresh one per call, unsafe for blind retries. Use a fresh key for each new heartbeat interval, not a replay of an old heartbeat. Cancellation may race a committed mutation: retry with that key or inspect the graph; do not assume rollback.

## Parking work

A hold parks work: it leaves the ready set, its status untouched, and comes back when the hold is released. Containment carries it, so parking a node parks everything under it, including work filed there later, and a parked node still blocks whatever depends on it. Unlike archiving a project, a hold leaves everything writable — parked work can still be planned, decomposed, and commented on.

Include the conditions for lifting the hold in `--reason`.

```sh
skift hold '#12' --reason 'resume when the roadmap is approved'
skift unhold '#12' --reason 'back on the roadmap'
skift project hold platform --reason 'resume when the spike lands'
skift project unhold platform --reason 'spike landed'
```

`explain` names every hold in the way, which node carries each one, and who placed it. A node comes back only when all of them are released; releasing a node's own hold leaves a hold on its parent alone.

**Focus is the complement.** It parks everything outside a keep-set in one call:

```sh
skift focus platform infra --reason 'this quarter'      # projects; everything else parked
skift -p platform focus '#12' --reason 'this week'      # nodes of one project
skift focus --clear --reason 'quarter over'
```

Without `--project` the sweep covers the organization and the keep-set names projects; with one it covers that project and names its nodes. Keeping a node keeps its whole lineage live, what it contains and what contains it, since parking a parent would park the node.

A sweep replaces rather than stacks: focusing again in a scope releases the sweep already there in the same transaction, so `--clear` never needs to be told which sweep it means. A sweep parks only what is not parked already and releases only what it parked, so a hold placed on its own outlives both.

On an empty first page, `skift ready` names holds affecting matching work and summarizes which tickets are waiting. A missing hold summary does not mean the project is finished: other blockers or the request's filters can still leave the queue empty.

## References and settings

| Reference | Resolves to |
|---|---|
| `#7` | node 7 of the current project |
| `slug#7` | node 7 of another project in this organization |
| `org/slug#7` | fully qualified |

The current project comes from `--project` / `-p`, `$SKIFT_PROJECT`, or the config file. A bare `#7` with no current project is an error, so qualify it. Native HTTP has no server-global current project or session: supply `projects` or `project` where the schema offers it, and qualified references otherwise. The native endpoint is `https://<canonical-node-dns-name>[:port]/mcp`, not a Connect `--address`. For the verified modern client, protocol metadata and TLS checks see [native setup](../../../../../docs/native-mcp-https.md).

| Setting | Resolution order |
|---|---|
| socket | `--socket`, `$SKIFT_SOCKET`, then `$XDG_RUNTIME_DIR/skift/skift.sock` when it exists, then `/run/skift/skift.sock`; on macOS without `$XDG_RUNTIME_DIR`, `$XDG_STATE_HOME/skift/skift.sock`, `~/.local/state` by default |
| remote service | `--address`, `$SKIFT_ADDRESS` — wins over the socket |
| config file | `--config`, `$SKIFT_CONFIG`, then `$XDG_CONFIG_HOME/skift/config.json` |

## When skift cannot be reached

If a tool or command fails before the service answers — `skift` not found, a service that cannot be reached, an unauthenticated caller, or an MCP tool whose error names `skift doctor` — run `skift doctor --output json` in a shell. It checks the binary on `PATH`, the configuration, the service, the MCP server and the skills, and every check that is not `ok` carries a `fix`. Tell the person what failed and what the fix is, and with their go-ahead apply it yourself rather than handing them commands to run. A fix on the server, such as binding a workstation to a principal, is theirs or their administrator's. After a fix to `PATH` or the config file, the MCP server needs a new session to pick it up.

Priorities `p0` (most urgent) to `p4`, default `p2`. Statuses `open`, `done`, `dropped`. Drop reasons `superseded`, `not_viable`, `no_longer_relevant`, and `other`, which needs a `--detail`.

## What work costs

An estimate says what a node is expected to cost, in tokens and with a basis. It needs no lease and changes neither the node nor its readiness, so estimate the work you file as you file it. Estimating again adds a record; the node's current estimate is the most recent, and `skift node show` carries it beside the rollup of what the node and everything under it has actually cost.

Reporting what an attempt *did* cost is the orchestrator's job, since the process running you counts your tokens. There is no MCP tool for it.

## Identity

You are identified by the transport, never by anything you send: on a unix socket by the uid on the other end, over a tailnet by the node's identity. No request field sets the principal, so provenance cannot be forged and events record who actually called.

What a principal may do is the service's policy, not something to predict from here. Make the call; a refusal is an error naming the rule and what to do instead.

A caller whose identity maps to no principal is rejected as unauthenticated. If every call fails that way, the caller has no principal — an administrative fix on the service, not something to work around in the client.

Tailnet identity uses the client machine's stable node ID. All its processes share the mapped principal; if user-mapped, agents inherit its gate/hold authority. MCP clientInfo, `harness`, `agent_ref`, `session_ref`, and proxy headers cannot select a principal. This is not per-agent isolation. Native HTTP is tailnet-only, not public OAuth; a hosted client needs actual private tailnet reachability and a mapped node, not just an HTTPS URL.
