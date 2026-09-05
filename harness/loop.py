#!/usr/bin/env python3
"""harness/loop.py — drive backlog.toml to a working release.

One node per iteration. An implementer agent builds it, a reviewer agent decides
whether the node's acceptance criteria and Gherkin scenarios are satisfied. On a
pass the harness commits and pushes. On a fail the reviewer's findings go back to
the implementer, up to a retry cap.

The reviewer judges the contract only. It is explicitly not a code reviewer.

    mise exec -- python harness/loop.py                 run until the backlog drains
    mise exec -- python harness/loop.py --effort high   more reasoning per node
    mise exec -- python harness/loop.py --max 3         a few iterations
    mise exec -- python harness/loop.py --dry-run       show what it would do
    mise exec -- python harness/loop.py --node 2        force one node

Exit codes: 0 drained, 1 error or cap reached, 2 blocked.
"""

import argparse
import asyncio
import json
import os
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "scripts"))
import ready as backlog  # noqa: E402

from claude_agent_sdk import (  # noqa: E402
    AssistantMessage,
    ClaudeAgentOptions,
    ResultMessage,
    TextBlock,
    ToolUseBlock,
    query,
)

ROOT = Path(__file__).resolve().parent.parent
BACKLOG = ROOT / "backlog.toml"
STATE = ROOT / "harness" / "state.json"
STOP = ROOT / "harness" / "STOP"
LOGS = ROOT / "harness" / "logs"

# The implementer must not grade its own homework or rewrite the spec. The
# design docs are protected; operational docs under docs/how-to/ are the
# implementer's to write, since a work order may name one as a deliverable.
PROTECTED = ("backlog.toml", "harness/", "scripts/ready.py", "AGENTS.md", "CLAUDE.md",
             "docs/vision.md", "docs/core-principles.md", "docs/domain-model.md",
             "docs/architecture.md", "docs/roadmap.md", "docs/adrs/", "docs/research/")

# The harness writes these itself while an agent is running, so they are never
# evidence that the implementer went out of bounds.
HARNESS_RUNTIME = ("harness/state.json", "harness/logs/", "harness/STOP", "harness/.venv/")

IMPLEMENTER_PROMPT = """\
You are implementing exactly one node of the dagger backlog, in a repository you
are seeing for the first time.

Before writing code, read the documents the work order references. They are the
authority: docs/domain-model.md, docs/architecture.md, docs/core-principles.md,
and the ADRs under docs/adrs/. The work order tells you which matter.

Your job is to satisfy the node's contract:

- Every acceptance criterion must hold.
- Every Gherkin scenario must exist as an executable spec. Ginkgo and Gomega,
  in the package the code lives in. The scenario text becomes the spec name.
- `make lint` and `make test` must pass when you are done. If the node is early
  enough that those targets do not exist yet, create them.

Rules:

- Do not modify backlog.toml, anything under harness/, docs/, or scripts/.
  Those are the specification and the machinery driving you. If you believe one
  of them is wrong, say so in your final message and continue.
- Do not commit. The harness commits after review.
- Do not start work belonging to another node. If a dependency turns out to be
  missing, say so in your final message rather than building it.
- Prefer the standard library and the pinned toolchain. Add a dependency only
  when the work order names it.

Finish with a short summary: what you built, which files, and anything the
reviewer should look at first.
"""

REVIEWER_PROMPT = """\
You are a contract reviewer. You decide one thing: whether the work satisfies
the acceptance criteria and Gherkin scenarios of the node you are given.

You are NOT a code reviewer. Do not comment on style, naming, formatting, file
layout, abstraction, duplication, performance, or design taste. Do not suggest
improvements. Do not bikeshed. A criterion met by ugly code is met.

How to judge:

- Work through the acceptance criteria one at a time. For each, find the
  evidence: the code that implements it, and the spec that proves it.
- Work through the Gherkin scenarios one at a time. Each must exist as an
  executable spec whose assertions actually correspond to the Then clauses. A
  spec that is named after the scenario but asserts nothing is a fail.
- Run the suite yourself. `make test` and `make lint`. Read the output.
- A criterion you cannot verify is a fail, not a pass with a caveat.

Reasons to fail:

- A criterion or scenario is unimplemented, or implemented only in prose.
- A spec exists but does not assert what the scenario says.
- Tests do not pass, or do not run.
- The work touched backlog.toml, harness/, docs/, or scripts/.

End your response with exactly one line in this form, and nothing after it:

VERDICT: PASS

or

VERDICT: FAIL

If FAIL, the lines immediately before the verdict must list each unmet
criterion or scenario, one per line, starting with "- ", quoting the criterion
and stating what is missing. Be specific enough that the implementer can fix it
without guessing.
"""


def now():
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def load_state():
    if STATE.exists():
        return json.loads(STATE.read_text())
    return {"nodes": {}, "history": []}


def save_state(state):
    STATE.write_text(json.dumps(state, indent=2) + "\n")


def record(state, num, attempt, role, usage, turns, verdict):
    """One usage record per node, attempt and role. Same shape dagger stores."""
    state["history"].append({
        "at": now(), "node": num, "attempt": attempt, "role": role,
        "verdict": verdict, "turns": turns, "usage": usage,
    })


def entry_cost(entry):
    # Entries written before per-model usage was recorded carry only a total.
    return sum(u["cost_usd"] for u in entry.get("usage", [])) or entry.get("cost_usd", 0.0)


def total_cost(state):
    return sum(entry_cost(e) for e in state["history"])


def node_state(state, num):
    return state["nodes"].setdefault(str(num), {"status": "open", "attempts": 0})


def merged_nodes(state):
    """Backlog nodes with harness status merged in, for the readiness rules."""
    _, nodes = backlog.load(BACKLOG)
    backlog.validate(nodes)
    for num, node in nodes.items():
        status = node_state(state, num)["status"]
        # A node interrupted mid-flight is resumable, not stranded. Attempts are
        # preserved, so a crash costs one attempt rather than the whole node.
        node["status"] = "open" if status == "in_progress" else status
    return nodes


def ready_nodes(state):
    """Agent-claimable nodes, most important first. Mirrors dagger's ready rules."""
    nodes = merged_nodes(state)
    kids = backlog.children(nodes)
    out = []
    for num in sorted(nodes):
        if nodes[num].get("assignee_kind") == "user":
            continue
        if backlog.blockers(num, nodes, kids):
            continue
        out.append(num)
    out.sort(key=lambda n: (backlog.PRIORITIES.index(nodes[n].get("priority", "p2")), n))
    return out, nodes


def is_grouping(node, kids):
    """A node that groups work rather than being work.

    It has children and states no acceptance criteria, so there is nothing for
    an agent to satisfy and nothing for a reviewer to judge. The domain model
    says such a node becomes claimable when its children close; here the honest
    completion is bookkeeping, not an agent.
    """
    return bool(kids) and "## Acceptance criteria" not in node.get("body", "")


def git(*args, check=True):
    return subprocess.run(
        ["git", *args], cwd=ROOT, check=check, capture_output=True, text=True
    ).stdout.strip()


def working_tree_dirty():
    """True when the implementer changed something, ignoring harness bookkeeping."""
    for line in git("status", "--porcelain").splitlines():
        path = line[3:].strip().strip('"')
        if not any(path.startswith(p) for p in HARNESS_RUNTIME):
            return True
    return False


def tree_snapshot():
    """Paths already modified before an agent runs. Not the agent's doing."""
    return {line[3:].strip().strip('"') for line in git("status", "--porcelain").splitlines()}


def touched_protected(baseline=frozenset()):
    changed = git("status", "--porcelain").splitlines()
    hits = []
    for line in changed:
        if line[3:].strip().strip('"') in baseline:
            continue
        path = line[3:].strip().strip('"')
        if any(path.startswith(p) for p in HARNESS_RUNTIME):
            continue
        if any(path.startswith(p) for p in PROTECTED):
            hits.append(path)
    return hits


def head():
    return git("rev-parse", "HEAD")


def undo_agent_commits(before):
    """Keep the work, drop the commit. The harness owns the history.

    Only commits that are not on the remote are the agent's to undo. A commit
    that reached origin while the agent ran was made by someone else, and
    resetting it once diverged local history and broke every later push.
    """
    if head() == before:
        return
    on_origin = subprocess.run(
        ["git", "merge-base", "--is-ancestor", "HEAD", "origin/main"], cwd=ROOT
    ).returncode == 0
    if on_origin:
        print("  .. HEAD moved to a commit already on origin; leaving it", flush=True)
        return
    git("reset", "--soft", before)
    print("  .. agent committed; un-committed, harness owns history", flush=True)


async def run_agent(kind, system_prompt, prompt, log_path, args):
    """Run one agent to completion. Returns (final_text, cost, turns)."""
    options = ClaudeAgentOptions(
        system_prompt=system_prompt,
        cwd=str(ROOT),
        permission_mode="auto",
        setting_sources=["project"],
        model=args.model,
        effort=args.effort,
        max_turns=200,
    )
    if kind == "reviewer":
        # A reviewer that can edit is a reviewer that fixes what it should fail.
        options.disallowed_tools = ["Write", "Edit", "NotebookEdit"]

    texts, cost, turns, usage = [], 0.0, 0, []
    with open(log_path, "a") as log:
        log.write(f"\n===== {kind} @ {now()} =====\n")
        async for message in query(prompt=prompt, options=options):
            if isinstance(message, AssistantMessage):
                for block in message.content:
                    if isinstance(block, ToolUseBlock):
                        line = summarize_tool(block)
                        print(f"    {line}", flush=True)
                        log.write(f"  {line}\n")
                    elif isinstance(block, TextBlock):
                        texts.append(block.text)
            elif isinstance(message, ResultMessage):
                cost = message.total_cost_usd or 0.0
                turns = message.num_turns or 0
                # Recorded in the shape of dagger's own Usage entity: counts per
                # model, never currency. Cost is derived from a rate table.
                for model_name, mu in (message.model_usage or {}).items():
                    usage.append({
                        "model": model_name,
                        "input_tokens": mu.get("inputTokens", 0),
                        "output_tokens": mu.get("outputTokens", 0),
                        "cache_read_tokens": mu.get("cacheReadInputTokens", 0),
                        "cache_write_tokens": mu.get("cacheCreationInputTokens", 0),
                        "cost_usd": round(mu.get("costUSD", 0.0), 4),
                    })
                if message.is_error:
                    log.write(f"  ERROR: {message.result}\n")
        final = texts[-1].strip() if texts else ""
        log.write(f"--- final ---\n{final}\n")
    return final, cost, turns, usage


def summarize_tool(block):
    name, inp = block.name, block.input or {}
    if name == "Bash":
        return f"$ {str(inp.get('command',''))[:100]}"
    for key in ("file_path", "pattern", "path"):
        if key in inp:
            return f"{name}: {inp[key]}"
    return name


def parse_verdict(text):
    """Return (passed, feedback). Absent or malformed verdict is a fail."""
    lines = [ln.rstrip() for ln in text.strip().splitlines()]
    for line in reversed(lines):
        if line.strip().upper().startswith("VERDICT:"):
            passed = "PASS" in line.upper()
            feedback = "\n".join(ln for ln in lines if ln.strip().startswith("- "))
            return passed, feedback or text.strip()
    return False, "The reviewer returned no VERDICT line.\n\n" + text.strip()


def work_order_text(num, nodes, slug):
    import io
    from contextlib import redirect_stdout

    buf = io.StringIO()
    with redirect_stdout(buf):
        backlog.work_order(num, nodes, slug)
    return buf.getvalue()


async def do_node(num, nodes, slug, state, args):
    node = nodes[num]
    ns = node_state(state, num)
    kids = backlog.children(nodes)[num]

    if is_grouping(node, kids):
        print(f"\n  {slug}#{num}  {node['title']}  (grouping; children complete)", flush=True)
        ns["status"] = "done"
        record(state, num, 0, "harness", [], 0, "grouping")
        save_state(state)
        return True

    order = work_order_text(num, nodes, slug)
    LOGS.mkdir(parents=True, exist_ok=True)
    log_path = LOGS / f"{num:03d}-{node['title'][:40].replace('/', '-')}.log"

    print(f"\n{'=' * 70}\n  {slug}#{num}  {node['title']}\n{'=' * 70}", flush=True)

    feedback = None
    for attempt in range(ns["attempts"] + 1, args.attempts + 1):
        ns["attempts"] = attempt
        ns["status"] = "in_progress"
        save_state(state)
        print(f"\n  -- implement (attempt {attempt}/{args.attempts})", flush=True)

        prompt = f"# Work order\n\n{order}\n"
        if feedback:
            prompt += (
                "\n# A previous attempt was rejected\n\n"
                "The reviewer found these parts of the contract unsatisfied. "
                "Fix them. Do not start over; build on what is already there.\n\n"
                f"{feedback}\n"
            )
        before, baseline = head(), tree_snapshot()
        try:
            _, cost_i, turns_i, usage_i = await asyncio.wait_for(
                run_agent("implementer", IMPLEMENTER_PROMPT, prompt, log_path, args),
                timeout=args.timeout,
            )
        except asyncio.TimeoutError:
            print(f"  !! implementer exceeded {args.timeout}s", flush=True)
            undo_agent_commits(before)
            record(state, num, attempt, "implementer", [], 0, "timeout")
            feedback = f"- The previous attempt ran past {args.timeout}s and was stopped. Work in smaller steps."
            continue
        undo_agent_commits(before)

        protected = touched_protected(baseline)
        if protected:
            feedback = "- The implementation modified protected paths: " + ", ".join(
                protected
            ) + ". Revert them and satisfy the contract in application code only."
            print(f"  !! touched protected paths: {protected}", flush=True)
            for path in protected:
                git("checkout", "--", path, check=False)
            continue

        if not working_tree_dirty():
            feedback = "- The attempt produced no changes at all."
            print("  !! no changes produced", flush=True)
            continue

        print(f"\n  -- review (attempt {attempt}/{args.attempts})", flush=True)
        diff = git("diff", "--stat", "HEAD")
        review_prompt = (
            f"# Node under review\n\n{order}\n\n"
            f"# Changes in the working tree\n\n```\n{diff}\n```\n\n"
            "Inspect the working tree, run the suite, and judge the contract."
        )
        try:
            verdict_text, cost_r, turns_r, usage_r = await asyncio.wait_for(
                run_agent("reviewer", REVIEWER_PROMPT, review_prompt, log_path, args),
                timeout=args.timeout,
            )
        except asyncio.TimeoutError:
            print(f"  !! reviewer exceeded {args.timeout}s; treating as fail", flush=True)
            verdict_text, cost_r, turns_r, usage_r = "VERDICT: FAIL", 0.0, 0, []
        passed, feedback = parse_verdict(verdict_text)

        record(state, num, attempt, "implementer", usage_i, turns_i, "n/a")
        record(state, num, attempt, "reviewer", usage_r, turns_r,
               "pass" if passed else "fail")

        if passed:
            print(f"  == PASS  (${cost_i + cost_r:.2f}, {turns_i + turns_r} turns)", flush=True)
            ns["status"] = "done"
            save_state(state)
            commit_and_push(num, node, slug, args)
            return True

        print(f"  == FAIL\n{feedback}", flush=True)
        save_state(state)

    ns["status"] = "blocked"
    ns["feedback"] = feedback
    save_state(state)
    return False


def commit_and_push(num, node, slug, args):
    git("add", "-A")
    subject = f"{slug}#{num}: {node['title']}"
    body = (
        f"Implements {slug}#{num} from backlog.toml.\n\n"
        "Reviewed against the node's acceptance criteria and scenarios.\n\n"
        "Claude-Session: https://claude.ai/code/session_01SYqDtWFVHmx7NgvH7imxP5"
    )
    subprocess.run(
        ["git", "commit", "-m", subject, "-m", body], cwd=ROOT, check=True,
        capture_output=True, text=True,
    )
    print(f"  ++ committed: {subject}", flush=True)
    if not args.no_push:
        r = subprocess.run(["git", "push"], cwd=ROOT, capture_output=True, text=True)
        print("  ++ pushed" if r.returncode == 0 else f"  !! push failed: {r.stderr.strip()}",
              flush=True)


def report(state, project, nodes):
    """Per-node token and cost tally. Calibration data for future estimates."""
    slug = project.get("slug", "backlog")
    per = {}
    for e in state["history"]:
        a = per.setdefault(e["node"], {"in": 0, "out": 0, "cr": 0, "cw": 0,
                                       "cost": 0.0, "turns": 0, "attempts": 0})
        if not e.get("usage"):
            a["cost"] += e.get("cost_usd", 0.0)
        for u in e.get("usage", []):
            a["in"] += u["input_tokens"]
            a["out"] += u["output_tokens"]
            a["cr"] += u["cache_read_tokens"]
            a["cw"] += u["cache_write_tokens"]
            a["cost"] += u["cost_usd"]
        a["turns"] += e.get("turns", 0)
        a["attempts"] = max(a["attempts"], e.get("attempt", 0))

    print(f"\n{'=' * 96}\n  {slug}: per-node usage\n{'=' * 96}")
    print(f"{'node':<10} {'status':<8} {'try':>3} {'turns':>5} {'input':>9} {'output':>8} "
          f"{'cache rd':>10} {'cache wr':>9} {'cost':>8}  title")
    print("-" * 96)
    tot = {"in": 0, "out": 0, "cr": 0, "cw": 0, "cost": 0.0, "turns": 0}
    for num in sorted(per):
        a = per[num]
        for k in tot:
            tot[k] += a[k]
        st = state["nodes"].get(str(num), {}).get("status", "?")
        title = nodes[num]["title"][:30] if num in nodes else ""
        print(f"{slug + '#' + str(num):<10} {st:<8} {a['attempts']:>3} {a['turns']:>5} "
              f"{a['in']:>9,} {a['out']:>8,} {a['cr']:>10,} {a['cw']:>9,} "
              f"${a['cost']:>7.2f}  {title}")
    print("-" * 96)
    print(f"{'TOTAL':<10} {'':<8} {'':>3} {tot['turns']:>5} {tot['in']:>9,} {tot['out']:>8,} "
          f"{tot['cr']:>10,} {tot['cw']:>9,} ${tot['cost']:>7.2f}")

    billable = tot["in"] + tot["out"] + tot["cr"] + tot["cw"]
    done = sum(1 for v in state["nodes"].values() if v["status"] == "done")
    blocked = sorted(n for n, v in state["nodes"].items() if v["status"] == "blocked")
    print(f"\n  {done} nodes done" + (f", blocked: {', '.join(blocked)}" if blocked else ""))
    print(f"  {billable:,} tokens across every agent and role")
    print(f"  ${tot['cost']:.2f} at list prices; included in a subscription")
    if done:
        print(f"  ${tot['cost'] / done:.2f} and {billable // done:,} tokens per completed node")


async def main():
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("--max", type=int, default=100, help="max nodes this run")
    p.add_argument("--attempts", type=int, default=3, help="review attempts per node")
    p.add_argument("--model", default="claude-fable-5-1", help="model (default: Fable 5.1)")
    p.add_argument("--effort", default="medium",
                   choices=["low", "medium", "high", "xhigh", "max"],
                   help="reasoning effort (default: medium)")
    p.add_argument("--node", type=int, default=None, help="force one node")
    p.add_argument("--budget", type=float, default=None,
                   help="stop once total spend passes this many USD")
    p.add_argument("--timeout", type=int, default=3600,
                   help="seconds before one agent is abandoned (default: 3600)")
    p.add_argument("--dry-run", action="store_true", help="show the plan and exit")
    p.add_argument("--report", action="store_true", help="print per-node usage and exit")
    p.add_argument("--no-push", action="store_true", help="commit but do not push")
    args = p.parse_args()

    if STOP.exists():
        STOP.unlink()
    state = load_state()
    project, _ = backlog.load(BACKLOG)
    slug = project.get("slug", "backlog")

    if args.report:
        _, all_nodes = backlog.load(BACKLOG)
        report(state, project, all_nodes)
        return 0

    if args.dry_run:
        order, nodes = ready_nodes(state)
        done = sum(1 for v in state["nodes"].values() if v["status"] == "done")
        print(f"{slug}: {done} done, {len(order)} ready of {len(nodes)} total\n")
        for num in order:
            print(f"  {nodes[num].get('priority','p2')}  {slug}#{num}  {nodes[num]['title']}")
        return 0

    total = 0.0
    for i in range(1, args.max + 1):
        order, nodes = ready_nodes(state)
        if args.node is not None:
            order = [args.node] if i == 1 else []
        if not order:
            remaining = [n for n, v in state["nodes"].items() if v["status"] == "blocked"]
            if remaining:
                STOP.write_text(f"BLOCKED: nodes {sorted(remaining)} exhausted their attempts\n")
                print(f"\nBLOCKED: {sorted(remaining)}")
                report(state, project, nodes)
                return 2
            STOP.write_text("DONE: no agent-claimable nodes remain\n")
            print("\nDONE: backlog drained")
            report(state, project, nodes)
            return 0

        ok = await do_node(order[0], nodes, slug, state, args)
        total = total_cost(state)
        done = sum(1 for v in state["nodes"].values() if v["status"] == "done")
        print(f"\n  {done}/{len(nodes)} done, ${total:.2f} spent", flush=True)
        if args.budget and total >= args.budget:
            STOP.write_text(f"BLOCKED: budget ${args.budget} reached at ${total:.2f}\n")
            print(f"\nStopping: budget ${args.budget:.2f} reached")
            report(state, project, nodes)
            return 2
        if not ok:
            print(f"  {slug}#{order[0]} blocked after {args.attempts} attempts; moving on",
                  flush=True)
        time.sleep(2)

    print(f"\nReached --max ({args.max}). Total: ${total:.2f}")
    report(state, project, nodes)
    return 1


if __name__ == "__main__":
    try:
        sys.exit(asyncio.run(main()))
    except KeyboardInterrupt:
        print("\ninterrupted")
        sys.exit(1)
