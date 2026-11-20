# How to install skift

This guide shows how to set up skift from nothing: a server, the `skift` CLI on each workstation, and the skills and MCP server in your agent harness. It ends with `skift doctor`, which checks the workstation the way a new shell and a new agent session will find it.

Each step starts from what the machine already has and points at that tool's own documentation for anything that isn't skift. An agent following this guide for someone can run every command itself, once that person agrees.

## Choose where the server runs

One server holds the plan for every workstation. Pick the first row that fits:

| You have | Run the server |
|----------|----------------|
| A server someone else runs | Nothing to run. Get its address, and ask whoever runs it to [bind your workstation](#create-the-first-principal), then [install the CLI](#install-the-cli). |
| A Linux host with Docker or Podman | [In containers](#run-the-server-in-containers), joined to a [Tailscale](https://tailscale.com/docs) tailnet that every workstation also joins. |
| A Linux host that runs services under systemd | From the rpm: follow [How to deploy skift from the rpm](deploying.md), then [install the CLI](#install-the-cli). |
| Only a Mac, for yourself | [As a launch agent](#run-the-server-on-a-mac), on the same Mac you work on. |

## Run the server in containers

You need a container runtime that runs Compose files, such as [Docker Compose](https://docs.docker.com/compose/install/) or [Podman Compose](https://docs.podman.io/en/latest/markdown/podman-compose.1.html), and a [Tailscale](https://tailscale.com/docs) tailnet.

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

## Run the server on a Mac

This runs your own `skift serve` under launchd, reached over a socket in your home directory, for agents on the same Mac. Install the CLI first, as [the next section](#install-the-cli) describes, then come back here.

You need PostgreSQL that the service can reach. [PostgreSQL's macOS downloads](https://www.postgresql.org/download/macosx/) list the ways to get it, among them Homebrew and Postgres.app. Create a database for skift, and write down its URL, for example `postgres://<you>@localhost:5432/skift`.

Bring the database to the current schema, and make your own uid its first principal:

```sh
skift admin migrate --database-url <database url>
skift admin bootstrap "$(id -u)" <organization> --database-url <database url>
```

Download the launch agent, [ing.stead.skift.plist](../contrib/launchd/ing.stead.skift.plist), into `~/Library/LaunchAgents/`:

```sh
curl -fsSL -o ~/Library/LaunchAgents/ing.stead.skift.plist \
  https://raw.githubusercontent.com/steading-ai/skift/v<version>/contrib/launchd/ing.stead.skift.plist
```

launchd expands neither `~` nor variables, so edit three things in it: the path of `skift` (`command -v skift` prints it), the database URL, and your home directory in the log paths. Then load it:

```sh
launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/ing.stead.skift.plist
```

The service listens on `~/.local/state/skift/skift.sock`, where the CLI and `skift mcp` look by default, so a Mac that runs its own server needs no address configured. Its log is the file the agent names. Apple documents launch agents in [Creating Launch Daemons and Agents](https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPSystemStartup/Chapters/CreatingLaunchdJobs.html).

## Install the CLI

The CLI is also the MCP server, so an agent harness needs `skift` on `PATH` too, and a harness starts it outside your interactive shell. Pick the first row that fits:

| You have | Install skift |
|----------|---------------|
| [mise](https://mise.jdx.dev) | The command below. |
| [Homebrew](https://brew.sh), but not mise | Install mise with Homebrew, as [mise's installation guide](https://mise.jdx.dev/installing-mise.html#homebrew) describes, then the command below. |
| Neither | Install mise as [mise's getting started guide](https://mise.jdx.dev/getting-started.html) describes, then the command below. |

```sh
mise use -g packslip:github.com/steading-ai/skift
```

mise's [packslip backend](https://mise.jdx.dev/dev-tools/backends/packslip.html) verifies the release's signed manifest and pins who signed it. `mise use -g github:steading-ai/skift` installs the same archive through its [github backend](https://mise.jdx.dev/dev-tools/backends/github.html).

mise holds a release back until it is a day old, which is its [minimum release age](https://mise.jdx.dev/configuration/settings.html#minimum_release_age), so on the day of a release this command installs the one before it, or fails if there is none. To install a release sooner, name it:

```sh
mise use -g packslip:github.com/steading-ai/skift@<version>
```

mise puts installed tools on `PATH` only once it is activated in your shell. An interactive shell needs [`mise activate`](https://mise.jdx.dev/getting-started.html#activate-mise); an agent harness and anything else that starts `skift` outside one needs mise's [shims](https://mise.jdx.dev/dev-tools/shims.html) on `PATH` as well. `skift doctor`, at the end of this guide, checks the second.

A release's archives are on its [release page](https://github.com/steading-ai/skift/releases) for any other route; [How to verify a release](verifying-a-release.md) checks one by hand.

## Point the CLI at the server

A Mac running its own server needs nothing here. Otherwise put the server's address in `~/.config/skift/config.json`, where every process reads it, including an MCP server a harness starts:

```json
{"address": "http://<hostname>.<tailnet>.ts.net"}
```

`<hostname>` is the server's `SKIFT_TAILNET_HOSTNAME` and `<tailnet>` your [tailnet name](https://tailscale.com/docs/features/magicdns). The tailnet's [access rules](https://tailscale.com/docs/features/access-control) must let the workstation reach the server on port 80. `$SKIFT_ADDRESS` sets the same for one shell, and overrides the file.

Confirm the CLI reaches the server:

```sh
skift project list
```

A new server has no projects, so the list is empty. An `unauthenticated` error means this workstation is not bound to a principal: run the bootstrap command again with its stable id.

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

Open a new terminal, so the check sees what a fresh shell sees, and run:

```sh
skift doctor
```

It checks six things, and says how to fix each one that is not `ok`:

| Check | What it asks |
|-------|--------------|
| `binary` | Which skift this is, and its version. |
| `path` | Whether a login shell finds `skift`, as a harness starting `skift mcp` needs it to. |
| `config` | Which config file, socket or address the CLI uses, and where that came from. |
| `service` | Whether the server answers, and knows this workstation. |
| `mcp` | Whether `skift mcp` starts and lists its tools. |
| `skills` | Whether the skift skill is installed where a harness reads it. |

It exits non-zero when a check fails, and `skift doctor --output json` prints the same report for an agent to read.

Then start a new agent session and ask it:

```text
What is ready in skift?
```

It calls the `ready` tool of the skift MCP server, or runs `skift ready`, and reports the ready set. On a new server that set is empty. If the MCP server cannot reach the service, its tools say so and name `skift doctor`.

Stuck on a step this guide doesn't cover? Ask on the [Steading Discord](https://discord.gg/BjCu2vHgX4).
