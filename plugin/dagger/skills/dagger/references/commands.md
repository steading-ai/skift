# Commands

Every example is the `dg` CLI. Each MCP tool is the same command with the same
arguments and the same JSON response, so translating either way is mechanical.

Output is JSON whenever stdout is not a terminal, and on request with
`--output json`. Errors are a JSON object on stderr: `{"code":…, "message":…}`,
and the exit code is non-zero.

## Finding and reading

```sh
dg ready                          # what can be worked now, most urgent then oldest
dg ready --assignee-kind user     # what is waiting on a human
dg ready --kind bug --min-priority p1 --label backend
dg node show '#7'                 # one node: body, edges, result, lease, artifacts, comments
dg node list                      # the project's nodes, filtered
dg explain '#7'                   # why it is not ready; no reasons means it is
dg search 'socket "permission denied" -legacy'
dg project list                   # most recently active first
```

`ready`, `node list` and `search` return a **summary** of each node: its
reference (`slug#7`), title, kind, priority, status, labels, and the derived
`ready`/`blocked`/`claimed` flags. `node show` returns everything, body
included. `--view refs|summary|full` (the `view` argument of the MCP tools)
changes that: `--view full` on a listing carries every body, `--view refs` is
references and flags alone.

`search` is PostgreSQL websearch syntax: bare words, `"quoted phrases"`,
`-excluded`, and `OR`. It covers node titles, bodies, results and comments
across every project you can read unless `--project` narrows it, and skips
archived projects unless `--include-archived`.

`explain` returns structured reasons — blocking nodes, a live lease and its
expiry, waiting on a user — not prose. Prefer it over inferring from `show`.

## Taking and finishing

```sh
dg claim --min-priority p2 --kind task      # returns the node AND a lease id
dg claim --wait 30m                          # block until something matches
dg claim --wait                              # block indefinitely
dg heartbeat "$LEASE"                        # renew; any write by the holder also renews
dg complete '#7' --lease "$LEASE" --result 'What was produced, and where.'
dg complete '#7' --lease "$LEASE" --result - # result body from stdin
dg release "$LEASE" --reason gate --detail 'needs a decision on the schema'
```

The lease id is a fencing token: a write carrying a stale or expired one is
rejected, which is how a reclaimed node resists its previous holder.

The **result body is the point of finishing**. Downstream agents assemble their
context from the results of what blocked them, so say what was produced and
where it lives. A result of "done" throws that away.

Release reasons: `gate` (you filed a gate and stepped back), `crash`, `budget`,
`expired`, `preempted`. `complete` is recorded for you when you complete.

## Changing the graph

`apply_patch` is the only way to mutate. A patch is a list of operations applied
in one transaction and cycle-checked as a whole: if one operation is rejected,
nothing in the patch happened.

Operations: `create_node`, `update_node`, `drop_node`, `complete_node`,
`reopen_node`, `add_edge`, `remove_edge`, `attach_artifact`, `add_comment`,
`edit_comment`, `resolve_comment`.

**Aliases make one patch a whole subgraph.** A `create_node` declares an
`alias`; any later operation in the same patch refers to the new node by it,
with no round trip to learn its number:

```json
{"ops": [
  {"create_node": {"alias": "spike", "title": "Spike the parser", "kind": "spike",
                   "priority": "p1"}},
  {"create_node": {"alias": "impl", "title": "Implement it", "blocked_by": ["spike"]}},
  {"create_node": {"alias": "docs", "title": "Document it", "parent": "impl"}},
  {"add_edge":    {"from": "impl", "to": "docs"}}
]}
```

The response is what the patch did, not what it was sent: every node it
created or changed as a reference with its number, status and readiness
flags, the map from each alias to the reference it became, `event_count`, and
the `request_id` it was applied under. Bodies are never echoed and events are
not returned; `--view full` (or `view: "full"`) returns every field and the
events, and `dg events` reads them from the log at any time.

```sh
dg patch apply patch.json
dg patch apply -                  # from stdin
dg patch apply - --view full      # everything back, events included
```

Field names differ per operation. Most name their target `node`; `add_edge`
and `remove_edge` use `from` and `to`. Reparenting and reprioritising are
`update_node` fields (`parent`, `priority`):

```json
{"update_node":   {"node": "#7", "body": "…", "priority": "p0"}}
{"drop_node":     {"node": "#7", "reason": "superseded",
                   "detail": "folded into #9", "cascade": false}}
{"complete_node": {"node": "#7", "result": {"body": "…"}}}
{"add_comment":   {"node": "#7", "body": "…", "in_reply_to": "<comment id>"}}
```

A patch touching a node you hold a lease on must carry that `lease_id`.

For one small change the CLI builds the patch for you; each answers in the
same refs view, and takes the same `--view`:

```sh
dg create 'Title' --body - --kind bug --priority p1 --blocked-by '#3' --label api
dg create 'Decide the retention policy' --assignee-kind user   # files a gate
dg link '#3' '#7'                                              # #3 before #7
dg comment '#7' 'note' --in-reply-to '<id>'
```

Every write takes `--request-id` as an idempotency key. Retrying with the same
one returns the original response and writes no second set of events; retrying
with the same id and *different* content is rejected as a conflict.

## References and settings

| Reference | Resolves to |
|---|---|
| `#7` | node 7 of the current project |
| `slug#7` | node 7 of another project in this organization |
| `org/slug#7` | fully qualified |

The current project comes from `--project` / `-p`, `$DG_PROJECT`, or the config
file. A bare `#7` with no current project is an error, so qualify it.

| Setting | Resolution order |
|---|---|
| socket | `--socket`, `$DG_SOCKET`, then `/run/dagger/dg.sock` |
| remote service | `--address`, `$DG_ADDRESS` — wins over the socket |
| config file | `--config`, `$DG_CONFIG`, then `$XDG_CONFIG_HOME/dagger/config.json` |

Priorities `p0` (most urgent) to `p4`, default `p2`. Statuses `open`, `done`,
`dropped`. Drop reasons `superseded`, `not_viable`, `no_longer_relevant`.

## Identity

You are identified by the transport, never by anything you send: on a unix
socket by the uid on the other end, over a tailnet by the node's identity. No
request field sets the principal, so provenance cannot be forged and events
record who actually called.

A caller whose identity maps to no principal is rejected as unauthenticated. If
every call fails that way, the caller has no principal — an administrative fix
on the service, not something to work around in the client.
