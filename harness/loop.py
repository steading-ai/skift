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

# The implementer must not grade its own homework or rewrite the spec.
PROTECTED = ("backlog.toml", "harness/", "docs/", "scripts/ready.py")

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


def node_state(state, num):
    return state["nodes"].setdefault(str(num), {"status": "open", "attempts": 0})


def merged_nodes(state):
    """Backlog nodes with harness status merged in, for the readiness rules."""
    _, nodes = backlog.load(BACKLOG)
    backlog.validate(nodes)
    for num, node in nodes.items():
        node["status"] = node_state(state, num)["status"]
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


def git(*args, check=True):
    return subprocess.run(
        ["git", *args], cwd=ROOT, check=check, capture_output=True, text=True
    ).stdout.strip()


def working_tree_dirty():
    return bool(git("status", "--porcelain"))


def touched_protected():
    changed = git("status", "--porcelain").splitlines()
    hits = []
    for line in changed:
        path = line[3:].strip().strip('"')
        if any(path.startswith(p) for p in PROTECTED):
            hits.append(path)
    return hits


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

    texts, cost, turns = [], 0.0, 0
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
                if message.is_error:
                    log.write(f"  ERROR: {message.result}\n")
        final = texts[-1].strip() if texts else ""
        log.write(f"--- final ---\n{final}\n")
    return final, cost, turns


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
        _, cost_i, turns_i = await run_agent(
            "implementer", IMPLEMENTER_PROMPT, prompt, log_path, args
        )

        protected = touched_protected()
        if protected:
            feedback = "- The implementation modified protected paths: " + ", ".join(
                protected
            ) + ". Revert them and satisfy the contract in application code only."
            print(f"  !! touched protected paths: {protected}", flush=True)
            git("checkout", "--", *protected, check=False)
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
        verdict_text, cost_r, turns_r = await run_agent(
            "reviewer", REVIEWER_PROMPT, review_prompt, log_path, args
        )
        passed, feedback = parse_verdict(verdict_text)

        entry = {
            "at": now(),
            "node": num,
            "attempt": attempt,
            "verdict": "pass" if passed else "fail",
            "cost_usd": round(cost_i + cost_r, 4),
            "turns": turns_i + turns_r,
        }
        state["history"].append(entry)

        if passed:
            print(f"  == PASS  (${entry['cost_usd']}, {entry['turns']} turns)", flush=True)
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


async def main():
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("--max", type=int, default=100, help="max nodes this run")
    p.add_argument("--attempts", type=int, default=3, help="review attempts per node")
    p.add_argument("--model", default="claude-fable-5-1", help="model (default: Fable 5.1)")
    p.add_argument("--effort", default="medium",
                   choices=["low", "medium", "high", "xhigh", "max"],
                   help="reasoning effort (default: medium)")
    p.add_argument("--node", type=int, default=None, help="force one node")
    p.add_argument("--dry-run", action="store_true", help="show the plan and exit")
    p.add_argument("--no-push", action="store_true", help="commit but do not push")
    args = p.parse_args()

    if STOP.exists():
        STOP.unlink()
    state = load_state()
    project, _ = backlog.load(BACKLOG)
    slug = project.get("slug", "backlog")

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
                return 2
            STOP.write_text("DONE: no agent-claimable nodes remain\n")
            print("\nDONE: backlog drained")
            return 0

        ok = await do_node(order[0], nodes, slug, state, args)
        total = sum(h["cost_usd"] for h in state["history"])
        print(f"\n  running total: ${total:.2f}", flush=True)
        if not ok:
            print(f"  {slug}#{order[0]} blocked after {args.attempts} attempts; moving on",
                  flush=True)
        time.sleep(2)

    print(f"\nReached --max ({args.max}). Total: ${total:.2f}")
    return 1


if __name__ == "__main__":
    try:
        sys.exit(asyncio.run(main()))
    except KeyboardInterrupt:
        print("\ninterrupted")
        sys.exit(1)
