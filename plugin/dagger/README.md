# dagger plugin

Registers the dagger MCP server and ships two skills: one teaching an agent to
drive the graph through it, or through the `dg` CLI, and one for planning an epic
before it is filed.

## What it installs

| Component | Path | Effect |
|-----------|------|--------|
| MCP server | `.mcp.json` | a `dagger` server, run as `dg mcp` over stdio |
| Skill | `skills/dagger/SKILL.md` | `dagger:dagger`, model-invoked when the conversation touches a dagger graph |
| Skill | `skills/epic/SKILL.md` | `dagger:epic`, model-invoked when work is planned as an epic before it is filed |

## Requirements

`dg` must be on `PATH` and able to reach a running service. The plugin does not
install or build it — see the repository root for that. If `dg` is missing, the
MCP server fails to start and the skill still loads, teaching the CLI for a
binary that is not there.

Check before installing:

```sh
dg version && dg project list
```

`dg` finds the service through `--socket` / `$DG_SOCKET` (default
`/run/dagger/dg.sock`), or `--address` / `$DG_ADDRESS` for a service on another
machine over a tailnet or HTTPS. A caller whose identity maps to no principal is
rejected as unauthenticated; that is an administrative fix on the service, not a
client one.

## If you already registered `dg mcp` by hand

The plugin's server is registered **alongside** any you configured yourself, not
instead of it. Both connect, both spawn a `dg mcp` process, and every session
carries two copies of the same ten tools under different names:

| Registration | Tool names |
|--------------|------------|
| your own, e.g. in `~/.claude.json` | `mcp__dagger__<tool>` |
| this plugin | `mcp__plugin_dagger_dagger__<tool>` |

Keep one. Removing the hand-rolled registration is `claude mcp remove dagger`,
or deleting its entry from the settings file that declares it.

## Install

From this repository:

```sh
claude plugin marketplace add ./plugin
claude plugin install dagger@dagger
```

Then restart the session, or start a new one, so the MCP server is picked up.

Verify:

```sh
claude plugin list                 # dagger, enabled
claude plugin details dagger       # components and their token cost
```

In a session, `/mcp` lists the `dagger` server and its ten tools —
`apply_patch`, `claim`, `comment`, `complete`, `explain`, `heartbeat`, `ready`,
`release`, `search`, `show_node`.

## Developing

Load the plugin straight from the working tree, with no marketplace and nothing
installed. This is the iteration loop:

```sh
claude --plugin-dir ./plugin/dagger
```

A plugin loaded this way takes precedence over an installed one of the same
name for that session, so it shadows a released version while you work on it.

Validate the manifests, the skill's frontmatter, and the overall structure:

```sh
claude plugin validate ./plugin              # the marketplace
claude plugin validate ./plugin/dagger       # the plugin
claude plugin validate ./plugin --strict     # warnings are errors; for CI
```

`--json` emits the report as JSON with the same exit codes.

### The version is the cache key

Installing copies the plugin into `~/.claude/plugins/cache/<marketplace>/<plugin>/<version>/`.
Editing the source afterwards does **not** update that copy, and
`claude plugin update` reports "already at the latest version" while the
installed skill stays as it was. Bump `version` in both manifests to publish a
change, or work from `--plugin-dir`, which reads the tree directly.

Bump `version` in **both** `.claude-plugin/plugin.json` and the marketplace's
entry for it; `claude plugin tag` checks that they agree before tagging a
release.

## Uninstall

```sh
claude plugin uninstall dagger
claude plugin marketplace remove dagger
```
