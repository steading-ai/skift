# Harness

Drives `backlog.toml` to a working release. One node per iteration, two agents.

```
mise exec -- python harness/loop.py --dry-run    what would run next
mise exec -- python harness/loop.py              run until the backlog drains
mise exec -- python harness/loop.py --node 2     force one node
```

## How a node is worked

1. The ready set is computed from `backlog.toml` merged with `harness/state.json`,
   using dagger's own readiness rules: open, unblocked, no open children, and
   never a node assigned to a user.
2. The **implementer** receives the work order and the documents it references.
   It may not touch `backlog.toml`, `harness/`, `docs/`, or `scripts/`; those are
   the specification and the machinery driving it. Violations are reverted and
   fed back as a review failure.
3. The **reviewer** receives the same work order and the working tree. It decides
   one thing: whether the acceptance criteria and Gherkin scenarios are satisfied.
   It cannot write files, so it cannot fix what it should fail. It is explicitly
   not a code reviewer and is instructed not to bikeshed.
4. `VERDICT: PASS` commits and pushes. `VERDICT: FAIL` returns the unmet criteria
   to the implementer, up to `--attempts` times. A node that exhausts its attempts
   is marked blocked and the loop moves on.

## State

`harness/state.json` holds status, attempt counts, and a cost history.
`backlog.toml` stays pristine, so the specification and the run are separable.
Deleting the state file restarts from scratch; editing one node's status reruns
just that node.

`harness/STOP` is written on exit: `DONE:` when the backlog drains, `BLOCKED:`
when nodes exhausted their attempts.

## Defaults

Fable 5.1 at medium effort, three attempts per node, permission mode `auto`.
Logs land in `harness/logs/`, one file per node.
