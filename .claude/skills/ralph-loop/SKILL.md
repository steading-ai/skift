---
name: ralph-loop
description: Use when authoring, validating, or running this repo's autonomous backlog loop — writing work orders in backlog.toml, decomposing work into a task DAG agents can execute unattended, or driving harness/loop.py. Encodes how to size nodes, write bodies for an agent with no context, and keep humans off the critical path.
argument-hint: "[status | author | add <what> | validate | run [--budget N] | diagnose [node]]"
allowed-tools:
  - Read
  - Edit(backlog.toml)
  - Glob
  - Grep
  - Bash(scripts/ready.py:*)
  - Bash(mise exec -- python harness/loop.py --dry-run:*)
  - Bash(mise exec -- python harness/loop.py --report:*)
  - Bash(git status:*)
  - Bash(git log:*)
  - Bash(git diff:*)
---

# Ralph Loop

**Autonomy:** model-invocable · authors, validates, and diagnoses without asking · launching a run is the one act needing an explicit instruction, because it runs for hours, spends plan budget, and pushes commits

## Arguments

```
$ARGUMENTS
```

## Instructions

Route on the first word. Empty means `status`.

### `author` · `add <what>`

Write or extend `backlog.toml`. Read `WORK-ORDER.md` in this directory before writing a body; it has the anatomy, a worked example, and sizing.

Decompose by **dependency and verifiability**, never by granularity. A node is one coherent deliverable a fresh agent can finish and a reviewer can judge in a single pass. Every agent pays a large fixed cost re-reading the design before it writes anything, so twice as many nodes costs nearly twice as much for the same code.

Four rules, each of which cost a wasted run to learn:

1. **Nobody waits on a human.** A node with `assignee_kind = "user"` stalls everything behind it. If the work needs a decision, *make the decision now* and write it into the body. A user node is legal only as a terminal leaf that blocks nothing.
2. **A body is self-contained.** The agent opens with an empty context window and sees only this repo and that body. Name the documents to read, the files to create, and the traps. Never write "as we discussed" or "per the above."
3. **A deliverable never sits in a protected path.** `PROTECTED` in `scripts/ready.py` lists them. A work order whose output is `docs/domain-model.md` cannot be satisfied and burns every attempt.
4. **Grouping nodes carry no contract.** A node with children and no acceptance criteria is bookkeeping the harness closes itself. Give a node children or a contract, never both.

### `validate`

`scripts/ready.py --lint`. It checks cycles, dangling references, missing criteria and scenarios, deliverables in protected paths, humans blocking work, and nodes that can never become ready. Every finding is a runtime failure you are paying to discover early. Fix all of them.

### `run [flags]`

Validate first, state the node count and rough cost from the calibration table in `WORK-ORDER.md`, then:

```
mise exec -- python harness/loop.py --budget <cap>
```

Launch only on an explicit instruction to run. If the ask is ambiguous, confirm — the single place in this skill to stop. The run command is deliberately absent from this skill's allowed tools, so in default mode it prompts; the two read-only harness forms do not. **While a run is live, do not commit to the working tree.** The harness records HEAD before each agent; a foreign commit diverges history and breaks every later push.

### `status` · `diagnose [node]`

`mise exec -- python harness/loop.py --dry-run` for what is claimable now, and `--report` in place of `--dry-run` for per-node tokens and cost. Both read `harness/state.json`. `scripts/ready.py` is state-blind, reading only `backlog.toml`: use `--all` for why a node is structurally not ready and `--show N` for one work order.

A blocked node keeps its last reviewer feedback in `harness/state.json`, and `harness/logs/<nnn>-*.log` holds both transcripts. Read the feedback before the code. Fix the **work order** when the contract was wrong or unsatisfiable; fix the code only when the contract was right and the work missed it.

## After a run

A green suite is not proof. Drive the system by hand as the principal your vision describes. The first run here passed 596 specs while agents could not file work at all, because the acceptance suite filed as a user and only ever claimed as the agent. Ten minutes of dogfooding found what 43 reviewed nodes did not.
