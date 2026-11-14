# Agent skills and the MCP server

An agent uses skift through two things: the skills, which teach it the graph, and the MCP server, which hands it the graph's operations as tools. Both live in this repository under [plugin/skift](../plugin/skift), and neither is tied to one harness.

## The skills

| Skill | Teaches | Source |
|-------|---------|--------|
| `skift` | Taking, doing and finishing work in a graph: claiming a node, reading its inputs, completing it with a result, what makes a node ready, and the mistakes that build the wrong graph. Covers the MCP server's tools and the `skift` CLI. | [plugin/skift/skills/skift](../plugin/skift/skills/skift) |
| `epic` | Planning work before it is filed: the epic document, one Markdown file per node, and how a directory of them becomes one patch. | [plugin/skift/skills/epic](../plugin/skift/skills/epic) |

Both are [Agent Skills](https://agentskills.io): a directory holding a `SKILL.md` whose frontmatter uses only the fields the [specification](https://agentskills.io/specification) defines. Any harness that reads Agent Skills can load them.

`epic` refers to files in `skift` by relative path, so install the two side by side.

The skills teach the `skift` CLI as well as the MCP server's tools, so they work where the server is not registered. Either way they need `skift` on `PATH` and a service it can reach.

## Install the skills

### As a Claude Code plugin

```bash
claude plugin marketplace add steading-ai/skift
claude plugin install skift@skift
```

The plugin carries both skills and registers the MCP server, so nothing else is needed. [The plugin's README](../plugin/skift/README.md) covers what it needs, how to check it worked, and how it is updated. Claude Code documents marketplaces under [Install and manage plugins](https://code.claude.com/docs/en/discover-plugins).

### With the `skills` CLI

```bash
npx skills add steading-ai/skift/plugin/skift
```

The [`skills` CLI](https://github.com/vercel-labs/skills) installs the skills you pick into the skills directory of each agent you pick, from [the agents it supports](https://github.com/vercel-labs/skills/blob/main/docs/agents.md). It installs skills only; the MCP server is registered separately.

The path after the repository's name matters. This repository also keeps skills for its own development under `.claude/skills/`, and `npx skills add steading-ai/skift` offers those alongside `skift` and `epic`.

### With mise

```bash
mise use packslip:github.com/steading-ai/skift
mise skills sync --dir .agents/skills
```

A release of skift declares both skills in its [packslip](https://packslip.dev) manifest, as [ADR-011](adrs/011-releases-are-immutable-and-attested.md) records. `mise use` installs the `skift` binary and fetches the skills that belong to that version; `mise skills sync` links them into the directory `--dir` names, which is whichever one your harness reads. The links point into the installed version, so run the sync again after changing versions. mise documents the commands and their settings under [Man pages, completions, and skills](https://mise.jdx.dev/dev-tools/packslip-resources.html).

mise holds a release back until it is a day old, which is its [minimum release age](https://mise.jdx.dev/configuration/settings.html#minimum_release_age). `mise use packslip:github.com/steading-ai/skift@<version>` installs a newer one by name.

This route installs skills only as well; the MCP server is registered separately.

## Register the MCP server

The MCP server is the `skift` binary, speaking MCP over stdio:

| Setting | Value |
|---------|-------|
| Transport | stdio |
| Command | `skift` |
| Arguments | `mcp` |

It reaches the service the way the CLI does, and reads the same settings from its environment: `SKIFT_SOCKET`, `SKIFT_ADDRESS`, `SKIFT_PROJECT` and `SKIFT_CONFIG`. It needs none of them when the service is on the default socket. `skift mcp --help` describes them.

Its tools are named after the commands they wrap: `ready`, `claim`, `inputs`, `complete` and the rest. A harness may list them under a prefix of its own. A lease claimed through a tool is renewed for as long as the server's process runs, and released when the harness closes it.

The Claude Code plugin registers the server itself, through [plugin/skift/.mcp.json](../plugin/skift/.mcp.json). Anywhere else, register a stdio server with that command and argument the way the harness documents:

| Harness | Skills | MCP servers |
|---------|--------|-------------|
| Claude Code | [Extend Claude with skills](https://code.claude.com/docs/en/skills) | [Connect Claude Code to tools via MCP](https://code.claude.com/docs/en/mcp) |
| Codex | [Build skills](https://learn.chatgpt.com/docs/build-skills) | [Model Context Protocol](https://learn.chatgpt.com/docs/extend/mcp) |
| OpenCode | [Skills](https://opencode.ai/docs/skills/) | [MCP servers](https://opencode.ai/docs/mcp-servers/) |
| Pi | [Skills](https://docs.pi.dev/latest/skills) | [MCP Servers](https://docs.pi.dev/latest/mcp) |
