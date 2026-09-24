# Sprint: prove the bet

**Theme:** stop building skift and start using it. The design session and first
build established that agents can build the thing. This sprint establishes
whether the thing is right.

**Reads first:** `docs/retrospectives/2026-09-05-first-build.md` for what was
learned, `docs/roadmap.md` for where M1 and M2 sit, and `/ralph-loop` for how
to author and run work orders. The sizing lesson from the retrospective applies:
fewer, larger nodes. This plan is about twelve, not forty-five.

## The four proofs

The bet has three open claims (retrospective, "Open bets"). Each proof below
retires one or produces the evidence against it.

| Proof | What it retires | Done when |
|---|---|---|
| **P1. Skift holds its own backlog** | "The model fits real work" | `backlog.toml` is deleted and `skift ready` answers what to build next |
| **P2. The harness dispatches from skift** | "Store plans, not work" is a real seam | `harness/loop.py` claims via `ClaimNext`, completes with a result, and never reads TOML |
| **P3. A gate reaches a human and unblocks work** | The vision's escalation story | An agent files a gate, a person is notified over Matrix, completes it, and the blocked node runs |
| **P4. Cost flows back and informs the next estimate** | The planning loop | Actual usage lands in skift per attempt, and the harness chooses model and effort from the node's estimate |

Nothing below is worth doing unless it serves one of these.

## Deployment is the precondition

All four proofs run against a real deployment, not the compose database. Skift
is a wellspring platform service in homestead terms: shared PostgreSQL, unix
socket reachable by grove, tsnet for the laptop. See `docs/architecture.md`.

There is currently **no way to create an organization, principal, or identity
without `psql`**. The acceptance suite seeds them with raw SQL and so did the
hand verification. That is the first real gap to close.

## The work

Twelve nodes. `blocked_by` is dependency; ordering within a wave is free.

| # | Node | Blocked by | Serves |
|---|---|---|---|
| 1 | `skift admin` bootstrap: create org, principal, and identity from the CLI, idempotent, with a `--from-uid` and `--from-tailscale-node` shorthand | | deploy |
| 2 | Homestead role: wellspring user, database on the shared PostgreSQL, migrations on deploy, `skift.service` and `skift-reap.timer` as user units, socket group readable by grove | 1 | deploy |
| 3 | tsnet enrollment: auth key from wellspring secrets, ACL tag, laptop reaches it; seed the laptop's tailscale identity as a user principal | 2 | deploy, P3 |
| 4 | Verify deployment by hand: laptop `skift mcp` over the tailnet, grove `skift` over the socket, same org, events show both identity schemes | 3 | deploy |
| 5 | TOML importer: `backlog.toml` to a patch stream, aliases preserved, one-time and idempotent by request id | 1 | P1 |
| 6 | Load this sprint into the deployed skift as its first project, then load the remaining M1 work; delete `backlog.toml` when `skift ready` is the source of truth | 4, 5 | P1 |
| 7 | Harness reads from skift: `ClaimNext --wait` replaces `ready.py`, work orders come from `skift node show`, completion carries the reviewer's verdict as the result body, `agent_ref` recorded | 6 | P2 |
| 8 | Reviewer fail path: plant a deliberately incomplete node, confirm rejection, feedback, and a converging retry. Zero failures across 43 nodes means this path has never run | 7 | P2 |
| 9 | Usage reporting: harness posts per-attempt usage through `report_usage`; a `skift estimate` command records estimates with a basis; `--report` reads from skift, not `state.json` | 7 | P4 |
| 10 | Model and effort by estimate: harness maps estimate bands to model and effort; a node with no estimate gets one from the median of comparable completed nodes | 9 | P4 |
| 11 | Notifier: a consumer of `WatchEvents` that posts user-assigned ready nodes to a Matrix room and marks them seen; homestead already runs the homeserver | 4 | P3 |
| 12 | Gate end to end on the real deployment: an agent working a node files a gate for a missing credential, the notifier fires, a person completes it from the laptop, the node runs | 8, 11 | P3 |

Waves at concurrency one: 1 → 2 → 3 → 4 → 5,6 → 7,11 → 8,9 → 10,12.
Nodes 5 and 11 can start as soon as 1 and 4 land, so a second worker is
useful from wave five onward.

## Deliberately not in this sprint

- **Worktree-per-node concurrency.** An optimization. Proofs come first.
- **Plan-limit-aware scheduling.** Needs P4's data to be worth doing.
- **`make schema` end-state dump.** Useful, not proof. Fold it into node 2 if
  the deploy role needs it, otherwise next sprint.
- **Semantic retrieval.** M3. kb becomes a consumer once P2 shows the stream
  is worth consuming.
- **Anything that adds a second organization.** M4.

## How to run it

1. Write nodes 1 through 12 as full work orders per `/ralph-loop author`, sized
   at the retrospective's guidance. Expect them larger than last time.
2. Node 6 is the pivot: before it, the harness runs from TOML as it did; after
   it, from skift. Run 1 through 5 with the existing harness.
3. Node 7 is the first thing that changes how work is dispatched. Run it, then
   node 8 immediately, because a harness that cannot fail a node cannot be
   trusted with the rest.
4. Hand-verify after every proof, as the principal the vision describes. The
   suite passed while agents could not file work. Do not let that happen twice.

## Done when

All four proofs hold on the homestead deployment, `backlog.toml` no longer
exists, and the next sprint is planned in skift.
