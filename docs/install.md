# How to install skift

This guide shows how to set up skift from nothing: a server, the `skift` CLI on each workstation, and the skills and MCP server in your agent harness. At the end, an agent on a workstation asks the server what is ready and gets an answer.

You need:

- A Linux host with a container runtime that runs Compose files, such as [Docker Compose](https://docs.docker.com/compose/), for the server.
- A [Tailscale](https://tailscale.com/docs) tailnet that the server and each workstation join. Workstations reach the server over it.
- [mise](https://mise.jdx.dev/getting-started.html) on each workstation.

## Run the server

On the host, download the compose file of the release you are installing and the settings template that goes with it. Use a [release](https://github.com/steading-ai/skift/releases) that lists `docker-compose.yaml` among its assets:

```sh
curl -fsSLO https://github.com/steading-ai/skift/releases/download/v<version>/docker-compose.yaml
curl -fsSL -o skift.env https://raw.githubusercontent.com/steading-ai/skift/v<version>/contrib/skift.env.example
```

The compose file pins the release's image as `<version>@sha256:<digest>`. Renovate and similar tools update that pair. To check the file and the image before running them, see [How to verify a release](verifying-a-release.md#check-the-image).

Fill in `skift.env`:

- `POSTGRES_PASSWORD`: letters and digits only. `openssl rand -hex 32` makes one.
- `SKIFT_TAILNET_HOSTNAME`: the name the server joins the tailnet under, for example `skift`.
- `TS_AUTHKEY`: an [auth key](https://tailscale.com/docs/features/access-control/auth-keys) that enrols the server on its first start.
- `SKIFT_TAILNET_TAGS`: the [tags](https://tailscale.com/docs/features/tags) the server advertises, if the key carries any.

Leave `SKIFT_IMAGE` empty: a release's compose file names its own image.

Start the stack:

```sh
docker compose --env-file skift.env up -d
```

Wait until the service's log says `database reachable; ready`:

```sh
docker compose --env-file skift.env logs skift
```

With podman, `podman compose` takes the same arguments. If the host already runs a Compose project named `skift`, such as the development database of a checkout, set `COMPOSE_PROJECT_NAME` to another name for every command in this guide.

On a host that runs its services under systemd, follow [How to deploy skift from the rpm](deploying.md) instead of this section and the next, then continue with [Install the CLI](#install-the-cli).

## Create the first principal

On a workstation that has joined the tailnet, print its node's stable id:

```sh
tailscale status --json | jq -r .Self.ID
```

On the host, once the service is ready, create the organization and its first principal, and bind that node to it:

```sh
docker compose --env-file skift.env run --rm skift \
  skift admin bootstrap 65532 <organization> --tailscale-node <stable id>
```

`65532` is the uid the service runs as in the image. Run the command again with another stable id for each further workstation; it adopts what exists and binds the new node to the same principal.

[Identity by transport](architecture.md#identity-by-transport) describes how a caller becomes a principal, and [Principal](domain-model.md#principal) what one is.

## Install the CLI

On each workstation:

```sh
mise use -g packslip:github.com/steading-ai/skift
```

mise's [packslip backend](https://mise.jdx.dev/dev-tools/backends/packslip.html) verifies the release's signed manifest and pins who signed it. `mise use -g github:steading-ai/skift` installs the same archive through its [github backend](https://mise.jdx.dev/dev-tools/backends/github.html). If mise reports no such version for a release published minutes ago, prefix the command with `MISE_USE_VERSIONS_HOST=0`, as its [troubleshooting page](https://mise.jdx.dev/troubleshooting.html#new-version-of-a-tool-is-not-available) describes.

Point the CLI at the server, in your shell's profile:

```sh
export SKIFT_ADDRESS=http://<hostname>.<tailnet>.ts.net
```

`<hostname>` is the server's `SKIFT_TAILNET_HOSTNAME` and `<tailnet>` your [tailnet name](https://tailscale.com/docs/features/magicdns). The tailnet's [access rules](https://tailscale.com/docs/features/access-control) must let the workstation reach the server on port 80. To set the address for every process, including an MCP server a harness starts outside your shell, put `{"address": "http://<hostname>.<tailnet>.ts.net"}` in `~/.config/skift/config.json` instead.

Confirm the CLI is installed and reaches the server:

```sh
skift version
skift project list
```

A new server has no projects, so the list is empty. An `unauthenticated` error means this workstation's node is not bound to a principal: run the bootstrap command again with its stable id.

## Connect your agent

[Agent skills and the MCP server](agent-skills.md) is the reference for everything in this section.

### Claude Code

```sh
claude plugin marketplace add steading-ai/skift
claude plugin install skift@skift
```

The plugin carries the skills and registers the MCP server. Start a new session afterwards. [The plugin's README](../plugin/skift/README.md) covers checking and updating it, and Claude Code documents plugins under [Install and manage plugins](https://code.claude.com/docs/en/discover-plugins).

### Any other harness

Install the skills with the [`skills` CLI](https://github.com/vercel-labs/skills):

```sh
npx skills add steading-ai/skift/plugin/skift
```

Or link the skills that belong to the installed binary into the directory your harness reads:

```sh
mise skills sync --dir <skills directory>
```

Then, if the harness speaks MCP, register a stdio server with the command `skift` and the argument `mcp`. A harness without MCP needs nothing more: the skills teach the `skift` CLI as well.

Each harness documents where its skills live and how it registers a server:

- Codex: [skills](https://learn.chatgpt.com/docs/build-skills), [MCP](https://learn.chatgpt.com/docs/extend/mcp)
- OpenCode: [skills](https://opencode.ai/docs/skills/), [MCP servers](https://opencode.ai/docs/mcp-servers/)
- Pi: [skills](https://docs.pi.dev/latest/skills), [MCP servers](https://docs.pi.dev/latest/mcp)

## Check it works

Ask the agent:

```text
What is ready in skift?
```

It calls the `ready` tool of the skift MCP server, or runs `skift ready`, and reports the ready set. On a new server that set is empty.
