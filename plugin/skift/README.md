# skift plugin

Registers the skift MCP server and ships two skills: one teaching an agent to
drive the graph through it, or through the `skift` CLI, and one for planning an epic
before it is filed.

## What it installs

| Component | Path | Effect |
|-----------|------|--------|
| MCP server | `.mcp.json` | a `skift` server, run as `skift mcp` over stdio |
| Skill | `skills/skift/SKILL.md` | `skift:skift`, model-invoked when the conversation touches a skift graph |
| Skill | `skills/epic/SKILL.md` | `skift:epic`, model-invoked when work is planned as an epic before it is filed |

## Requirements

`skift` must be on `PATH` and able to reach a running service. The plugin does not
install or build it — see the repository root for that. If `skift` is missing, the
MCP server fails to start and the skill still loads, teaching the CLI for a
binary that is not there.

Check before installing:

```sh
skift version && skift project list
```

`skift` finds the service through `--socket` / `$SKIFT_SOCKET` (default: your
own `skift serve` under `$XDG_RUNTIME_DIR` when it is running, otherwise the
system service's `/run/skift/skift.sock`), or `--address` / `$SKIFT_ADDRESS`
for a service on another machine over a tailnet or HTTPS. A caller whose identity maps to no principal is
rejected as unauthenticated; that is an administrative fix on the service, not a
client one.

## If you already registered `skift mcp` by hand

The plugin's server is registered **alongside** any you configured yourself, not instead of it. Both connect, both spawn a `skift mcp` process, and every session carries two copies of the same tools under different names:

| Registration | Tool names |
|--------------|------------|
| your own, e.g. in `~/.claude.json` | `mcp__skift__<tool>` |
| this plugin | `mcp__plugin_skift_skift__<tool>` |

Keep one. The skills pre-approve the read-only tools under the plugin's names, so a hand-rolled registration's copies are the ones Claude Code still asks about. Removing the hand-rolled registration is `claude mcp remove skift`, or deleting its entry from the settings file that declares it.

## Install

No checkout is needed. The marketplace manifest is at the root of the repository, so Claude Code reads it from GitHub:

```sh
claude plugin marketplace add steading-ai/skift
claude plugin install skift@skift
```

From a checkout, name its root instead: `claude plugin marketplace add ./` there, or the path to it from anywhere else.

Then restart the session, or start a new one, so the MCP server is picked up.

The plugin is one of three ways to install the skills, and the only one that also registers the MCP server. [Agent skills and the MCP server](../../docs/agent-skills.md) covers the others, and other harnesses.

Verify:

```sh
claude plugin list                 # skift, enabled
claude plugin details skift        # components and their token cost
```

In a session, `/mcp` lists the `skift` server and its tools, among them `ready`, `claim`, `inputs`, `complete` and `apply_patch`. [skills/skift/references/commands.md](skills/skift/references/commands.md) names all of them.

## Developing

Load the plugin straight from the working tree, with no marketplace and nothing
installed. This is the iteration loop:

```sh
claude --plugin-dir ./plugin/skift
```

A plugin loaded this way takes precedence over an installed one of the same
name for that session, so it shadows a released version while you work on it.

Validate the manifests, the skill's frontmatter, and the overall structure:

```sh
claude plugin validate .                     # the marketplace, at the repository root
claude plugin validate ./plugin/skift        # the plugin
claude plugin validate . --strict            # warnings are errors; for CI
```

`--json` emits the report as JSON with the same exit codes.

### The version is the cache key

Installing copies the plugin into `~/.claude/plugins/cache/<marketplace>/<plugin>/<version>/`.
Editing the source afterwards does **not** update that copy, and
`claude plugin update` reports "already at the latest version" while the
installed skill stays as it was. Bump `version` in both manifests to publish a
change, or work from `--plugin-dir`, which reads the tree directly.

Bump `version` in **both** `plugin/skift/.claude-plugin/plugin.json` and the plugin's entry in the marketplace manifest, `.claude-plugin/marketplace.json` at the repository root; `claude plugin tag` checks that they agree before tagging a release.

## Uninstall

```sh
claude plugin uninstall skift
claude plugin marketplace remove skift
```
