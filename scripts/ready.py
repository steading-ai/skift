#!/usr/bin/env python3
"""Compute the ready set for a flat-file backlog.

A paper test of dagger's domain model, run before there is a schema. Mirrors
the rules in docs/domain-model.md: a node is ready when it is open, has no
unfinished blocking predecessor, holds no live lease, and has no open children.

Usage:
    scripts/ready.py [backlog.toml]
    scripts/ready.py --all       show blocked nodes and why
    scripts/ready.py --show 18   print one node as a work order
    scripts/ready.py --lint      check the graph before spending a run on it
"""

import re
import sys
import tomllib

PRIORITIES = ("p0", "p1", "p2", "p3", "p4")

# Paths an implementer may not write: the specification it works from and the
# machinery driving it. The harness imports this. A work order that names a
# deliverable under one of these is unsatisfiable, and burns every attempt on
# the node before anyone notices.
PROTECTED = (
    "backlog.toml",
    "harness/",
    "scripts/",
    ".claude/",
    "AGENTS.md",
    "CLAUDE.md",
    "docs/vision.md",
    "docs/core-principles.md",
    "docs/domain-model.md",
    "docs/architecture.md",
    "docs/roadmap.md",
    "docs/adrs/",
    "docs/research/",
)


def load(path):
    with open(path, "rb") as fh:
        doc = tomllib.load(fh)
    nodes = {n["number"]: n for n in doc["nodes"]}
    if len(nodes) != len(doc["nodes"]):
        sys.exit("duplicate node numbers")
    return doc.get("project", {}), nodes


def validate(nodes):
    """Reject dangling references and cycles, the way a patch would."""
    for num, node in nodes.items():
        for ref in node.get("blocked_by", []) + ([node["parent"]] if "parent" in node else []):
            if ref not in nodes:
                sys.exit(f"#{num} references #{ref}, which does not exist")
        if node.get("priority", "p2") not in PRIORITIES:
            sys.exit(f"#{num} has priority {node['priority']}")

    colour = {}

    def visit(num, trail):
        state = colour.get(num)
        if state == "done":
            return
        if state == "open":
            cycle = " -> ".join(f"#{n}" for n in trail[trail.index(num):] + [num])
            sys.exit(f"cycle: {cycle}")
        colour[num] = "open"
        for ref in nodes[num].get("blocked_by", []):
            visit(ref, trail + [num])
        colour[num] = "done"

    for num in nodes:
        visit(num, [])


def children(nodes):
    out = {num: [] for num in nodes}
    for num, node in nodes.items():
        if "parent" in node:
            out[node["parent"]].append(num)
    return out


def blockers(num, nodes, kids):
    """Why this node is not ready. Empty means it is."""
    node = nodes[num]
    if node.get("status", "open") != "open":
        return [f"status is {node['status']}"]
    reasons = []
    for ref in node.get("blocked_by", []):
        if nodes[ref].get("status", "open") == "open":
            reasons.append(f"blocked by #{ref} {nodes[ref]['title']}")
    open_kids = [k for k in kids[num] if nodes[k].get("status", "open") == "open"]
    if open_kids:
        reasons.append(f"has {len(open_kids)} open child(ren): " + ", ".join(f"#{k}" for k in open_kids))
    if node.get("lease"):
        reasons.append(f"claimed by {node['lease']}")
    return reasons


def work_order(num, nodes, slug):
    """Everything an agent with an empty context window needs for one node."""
    node = nodes[num]
    print(f"# {slug}#{num}  {node['title']}\n")
    print(f"kind: {node.get('kind', 'task')}    priority: {node.get('priority', 'p2')}"
          f"    assignee: {node.get('assignee_kind', 'agent')}")
    if "parent" in node:
        print(f"parent: {slug}#{node['parent']}  {nodes[node['parent']]['title']}")
    deps = node.get("blocked_by", [])
    if deps:
        print("depends on:")
        for ref in deps:
            state = nodes[ref].get("status", "open")
            print(f"  {slug}#{ref}  [{state}]  {nodes[ref]['title']}")
    print()
    print(node.get("body", "").strip())


def deliverables(body):
    """The Deliverables section of a work order, or empty."""
    match = re.search(r"^## Deliverables\s*$(.*?)(?=^## |\Z)", body, re.M | re.S)
    return match.group(1) if match else ""


def drain(nodes, kids, with_humans):
    """Nodes reachable by repeatedly completing whatever is ready."""
    done = set()
    while True:
        step = [
            num for num, node in nodes.items()
            if num not in done
            and (with_humans or node.get("assignee_kind") != "user")
            and all(b in done for b in node.get("blocked_by", []))
            and all(c in done for c in kids[num])
        ]
        if not step:
            return done
        done |= set(step)


def lint(nodes):
    """Every check here maps to a failure that costs a run to discover."""
    problems, notes = [], []
    kids = children(nodes)

    for num in sorted(nodes):
        node, body = nodes[num], nodes[num].get("body", "")
        has_criteria = "## Acceptance criteria" in body

        # A node with children and no contract is bookkeeping the harness
        # completes itself. One with both is neither, and an agent claiming it
        # finds nothing to satisfy.
        if kids[num]:
            if has_criteria:
                problems.append(
                    f"#{num}: has children and acceptance criteria. A grouping node "
                    f"carries no contract; a working node has no children")
            continue

        if node.get("assignee_kind") == "user":
            continue
        if not has_criteria:
            problems.append(f"#{num}: no '## Acceptance criteria'. The reviewer has no contract to judge")
        if "```gherkin" not in body:
            problems.append(f"#{num}: no Gherkin scenarios. The specs have nothing to encode")

        for quoted in re.findall(r"`([^`\n]+)`", deliverables(body)):
            path = quoted.strip().lstrip("./")
            if any(path.startswith(prefix) for prefix in PROTECTED):
                problems.append(
                    f"#{num}: names `{path}` as a deliverable, inside a protected path. "
                    f"An implementer cannot write it")

    for num in sorted(nodes):
        if nodes[num].get("assignee_kind") != "user":
            continue
        blocks = sorted(n for n, v in nodes.items() if num in v.get("blocked_by", []))
        if blocks:
            problems.append(
                f"#{num}: assigned to a user and blocks {blocks}. The loop stalls until a "
                f"person answers. Decide now and write the decision into the body instead")

    without = drain(nodes, kids, with_humans=False)
    with_ = drain(nodes, kids, with_humans=True)
    for num in sorted(set(nodes) - with_):
        problems.append(f"#{num}: never becomes ready, even once every human node is closed")
    for num in sorted(with_ - without):
        if nodes[num].get("assignee_kind") != "user":
            notes.append(f"#{num}: waits on a human node, so the loop leaves it open")
    return problems, notes


def main():
    args = [a for a in sys.argv[1:] if not a.startswith("-")]
    show_all = "--all" in sys.argv
    do_lint = "--lint" in sys.argv
    show = None
    if "--show" in sys.argv:
        show = int(sys.argv[sys.argv.index("--show") + 1])
        args = [a for a in args if a != str(show)]
    project, nodes = load(args[0] if args else "backlog.toml")
    validate(nodes)
    kids = children(nodes)
    slug = project.get("slug", "backlog")

    if do_lint:
        problems, notes = lint(nodes)
        for note in notes:
            print(f"  note  {note}")
        for problem in problems:
            print(f"  FAIL  {problem}")
        print(f"\n{len(nodes)} nodes, {len(problems)} problem(s)"
              + (", none blocking" if not problems else ""))
        return 1 if problems else 0

    if show is not None:
        if show not in nodes:
            sys.exit(f"no node #{show}")
        work_order(show, nodes, slug)
        return 0

    ready, blocked = [], []
    for num in sorted(nodes):
        reasons = blockers(num, nodes, kids)
        (blocked if reasons else ready).append((num, reasons))

    ready.sort(key=lambda item: (PRIORITIES.index(nodes[item[0]].get("priority", "p2")), item[0]))

    print(f"{slug}: {len(ready)} ready, {len(blocked)} not ready, {len(nodes)} total\n")
    print("READY")
    for num, _ in ready:
        node = nodes[num]
        who = " [user]" if node.get("assignee_kind") == "user" else ""
        kind = f" ({node['kind']})" if node.get("kind") else ""
        print(f"  {node.get('priority', 'p2')}  {slug}#{num}{who}{kind}  {node['title']}")

    if show_all:
        print("\nNOT READY")
        for num, reasons in blocked:
            print(f"  {slug}#{num}  {nodes[num]['title']}")
            for reason in reasons:
                print(f"        {reason}")


if __name__ == "__main__":
    sys.exit(main() or 0)
