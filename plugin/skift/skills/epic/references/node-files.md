# Node files

One Markdown file per node. The frontmatter is a `create_node` operation, the
body is the node body, and a directory of them compiles to **one patch**. This is
how a decomposition is drafted and proofread: a work order is read as rendered
Markdown, and a patch with long bodies in JSON strings is not writable by hand.

## Layout

```
patches/
  README.md            skipped by the importer
  <project-slug>/      one directory per skift project
    <alias>.md         one node
```

A directory of `.md` files is **one project** — the current one. A directory of
subdirectories is **several**, each named by its project's slug, filed in one
patch. Both at once is an error. `README.md` is skipped.

The filename without `.md` is the node's **alias**, which is what other files
refer to it by. An `alias:` key overrides it. An alias may not contain `#`, `/`,
or whitespace.

## Frontmatter

YAML: a flat map of scalars and lists. Block and flow lists are both accepted,
because Obsidian rewrites one into the other when a property is edited in its
panel. Any key not in this table is an error naming the key and the file — so a
typo surfaces instead of silently dropping a field.

| Key | Becomes | Notes |
|---|---|---|
| `title` | `title` | required to create |
| `kind` | `kind` | free string; `task` by default, `bug`, `spike`, `chore` conventional |
| `priority` | `priority` | `p0`–`p4`; `p2` by default. Omit for the default |
| `parent` | `parent` | containment. Must be in the same project as the node |
| `blocked_by` | `blocked_by` | list. Dependency: each must finish first |
| `labels` | `labels` | list |
| `assignee_kind` | `assignee_kind` | `user` makes the node a **gate** |
| `assignee` | `assignee` | |
| `alias` | `alias` | defaults to the filename |
| `blocks` | `add_edge` ops | list. This node must finish before each — the reverse edge, for a node filed after the ones it precedes |
| `number` | — | **read-only.** Written once by import, and by export; says the node is filed |
| `status` | — | **read-only.** Written by export |

Enum values are the short lowercase forms: `p1`, `user`. Quote any title
containing `: ` and every numeric reference — bare `#7` is a YAML comment.

## References

`parent`, `blocked_by` and `blocks` each take:

| Shape | Means |
|---|---|
| `other-file` | an alias in the same directory |
| `slug#other-file` | the file `slug/other-file.md` in a sibling directory |
| `#7`, `slug#7` | a node already in the graph |

A `slug#name` resolves to that file's `number` when it has one, and otherwise to
the node the same patch is about to create. A `slug#name` with no such file is an
error naming the path it looked for.

## Body

Everything after the frontmatter, following
[../../skift/references/work-orders.md](../../skift/references/work-orders.md):
Context, Deliverables, Acceptance criteria, Scenarios, Notes. Write it for an
agent with an empty context window — it has none of the conversation that
produced it and no way to ask.

`%% … %%` is a review comment, stripped before filing: use it for notes to
yourself and to whoever proofreads. Runs of three or more blank lines collapse to
one, leading and trailing whitespace is trimmed, and the body ends in exactly one
newline. Export writes the same canonical form, so export followed by import is a
no-op.

## Filing

```sh
skift patch import patches/skift --dry-run   # the patch it would apply; writes nothing
skift patch import patches/skift             # one patch, then writes each number back
```

On success, import writes `number: N` into each file it created. That is the only
key it ever writes, and it writes it once. A file carrying `number` is an
**update**: a second run sends only the fields that differ, and nothing at all
when nothing differs. So fixing a typo and re-running is an edit, not fourteen
duplicate nodes.

Import never removes. No `remove_edge`, no drop, no status change: an edge an
agent added that is absent from your file is reported and left alone. Removing
one is a `remove_edge` op in a patch, by hand, with intent.

A file without `number` whose title matches an open node in its project is
refused, naming both — the guard against re-filing a decomposition whose numbers
were lost. `--allow-duplicates` lifts it.

If the apply succeeded and the write-back failed, **re-run before editing
anything** — the same files replay the same patch and the numbers land. Edit
first and you get a conflict instead.

> Neither `skift patch import` nor `skift node export` has shipped. Until they do,
> assemble the directory into a patch and `skift patch apply` it, and read the
> store with `skift node show`. The format above is final; this paragraph is the
> only thing that changes.

## Reading it back

```sh
skift node export '#7'                            # one node, in this dialect, to stdout
skift node export '#1' --children --dir out       # a subtree as files; --force to overwrite
skift patch import out --dry-run                  # proves the draft matches the graph
```

Export is a rendering, never an input: it is how you proofread what is in the
store after agents have changed it. To take ownership of a node someone else
filed, export it once into your drafts as a copy. Nothing syncs in either
direction, and no command reconciles the two.

## Traps

- **A file with `number` is an update.** Renaming or moving it does not make a new
  node; deleting the key to "re-file" it makes a duplicate.
- **Edge direction is "must finish before".** Reversed, you get a valid graph that
  is the wrong one, and nothing errors.
- **A gate stalls everything that depends on it** until a person completes it,
  and dropping it does not unblock. Put the agent work that prepares it in a separate node the
  gate depends on.
- **Giving a node a child takes it out of the ready set** until every child
  closes.
- **`parent` stays inside one project.** An epic that spans projects is a grouping
  node in each and an edge between them.
- **Ready means nothing is left undone about it:** open, unblocked, unheld,
  unleased, and no open children. Check the shape has at least one node that
  qualifies on day one.
