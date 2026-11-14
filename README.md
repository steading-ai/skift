# Skift

Agent-first project management built on directed acyclic graphs.

A software project is a graph of tasks. When every dependency of a task is satisfied, the task is ready, and a ready task is something an agent can start on. Skift is the durable store for that graph: agents and humans build and mutate a backlog through one API, and orchestrators consume the set of ready work to dispatch agents in parallel.

Skift holds the plan. It does not run agents, schedule work, or define workflows. See the [vision](docs/vision.md) for why, the [core principles](docs/core-principles.md) that guide the work, and the [domain model](docs/domain-model.md) for details on how skift represents the graph.

## Install

With [mise](https://mise.jdx.dev), which verifies the release's signature and pins who signed it:

```sh
mise use -g packslip:github.com/steading-ai/skift
```

mise installs a release once it is a day old; add `@<version>` to install a newer one.

Or download an archive from the [releases](https://github.com/steading-ai/skift/releases) and check that this repository's release workflow built it, as [How to verify a release](docs/verifying-a-release.md) describes:

```sh
gh attestation verify skift_*.tar.gz --repo steading-ai/skift --signer-workflow steading-ai/skift/.github/workflows/release.yml
```

That is the CLI and the MCP server, which call a running service. [How to install skift](docs/install.md) is the full path: a server, the CLI on each workstation, and the skills in your agent harness. [How to deploy skift from the rpm](docs/deploying.md) runs the server under systemd instead of in containers.

## Status

Skift is a working service, CLI, and MCP server, and its own backlog is held in skift. The service runs on Linux with PostgreSQL, and callers reach it over a unix socket or a tailnet. Releases carry binaries for Linux on amd64 and arm64 and for macOS on arm64.

It is pre-1.0: a minor version may change the interface. The [roadmap](docs/roadmap.md) has the milestones and what is deliberately deferred.

## Documentation

To get something done:

- [How to install skift](docs/install.md): a server, the CLI, and a working agent
- [How to deploy skift from the rpm](docs/deploying.md)
- [How to verify a release](docs/verifying-a-release.md)
- [How to develop skift](docs/developing.md)
- [How to cut a release](docs/releasing.md)

To look something up:

- [Domain model](docs/domain-model.md): nodes, edges, readiness, holds, leases, patches, events
- [Agent skills and the MCP server](docs/agent-skills.md): installing the skills into an agent harness and registering the server
- [CI](docs/ci.md): the workflows, the image, and dependency updates

To understand why:

- [Vision](docs/vision.md)
- [Core principles](docs/core-principles.md)
- [Architecture](docs/architecture.md)
- [Architecture decision records](docs/adrs/)
- [Roadmap](docs/roadmap.md)

The [security policy](SECURITY.md) says how to report a vulnerability and which releases get fixes. The [archive](archive/README.md) holds retired machinery and dated records, unmaintained.

## License

Skift is released under the [ISC license](LICENSE.md).
