#!/usr/bin/env python3
"""Compute the ready set for a flat-file backlog.

A paper test of dagger's domain model, run before there is a schema. Mirrors
the rules in docs/domain-model.md: a node is ready when it is open, has no
unfinished blocking predecessor, holds no live lease, and has no open children.

Usage:
    scripts/ready.py [backlog.toml]
    scripts/ready.py --all       show blocked nodes and why
    scripts/ready.py --show 18   print one node as a work order
"""

import sys
import tomllib

PRIORITIES = ("p0", "p1", "p2", "p3", "p4")


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


def main():
    args = [a for a in sys.argv[1:] if not a.startswith("-")]
    show_all = "--all" in sys.argv
    show = None
    if "--show" in sys.argv:
        show = int(sys.argv[sys.argv.index("--show") + 1])
        args = [a for a in args if a != str(show)]
    project, nodes = load(args[0] if args else "backlog.toml")
    validate(nodes)
    kids = children(nodes)
    slug = project.get("slug", "backlog")

    if show is not None:
        if show not in nodes:
            sys.exit(f"no node #{show}")
        work_order(show, nodes, slug)
        return

    ready, blocked = [], []
    for num in sorted(nodes):
        reasons = blockers(num, nodes, kids)
        (blocked if reasons else ready).append((num, reasons))

    ready.sort(key=lambda item: (PRIORITIES.index(nodes[item[0]].get("priority", "p2")), item[0]))

    print(f"{slug}: {len(ready)} ready, {len(blocked)} not ready, {len(nodes)} total\n")
    print("READY")
    for num, _ in ready:
        node = nodes[num]
        who = " [human]" if node.get("assignee_kind") == "human" else ""
        kind = f" ({node['kind']})" if node.get("kind") else ""
        print(f"  {node.get('priority', 'p2')}  {slug}#{num}{who}{kind}  {node['title']}")

    if show_all:
        print("\nNOT READY")
        for num, reasons in blocked:
            print(f"  {slug}#{num}  {nodes[num]['title']}")
            for reason in reasons:
                print(f"        {reason}")


if __name__ == "__main__":
    main()
