# Dagger

Agent-first project management built on directed acyclic graphs.

> [!NOTE]
> Working title. The name collides with [dagger.io](https://dagger.io) and will change before the project is public.

A software project is a graph of tasks. When every dependency of a task is satisfied, the task is ready, and a ready task is something an agent can start on. Dagger is the durable store for that graph: agents and humans build and mutate a backlog through one API, and orchestrators consume the set of ready work to dispatch agents in parallel.

Dagger holds the plan. It does not run agents, schedule work, or define workflows. See [docs/vision.md](docs/vision.md) for why, [docs/core-principles.md](docs/core-principles.md) for the decision filters, and [docs/domain-model.md](docs/domain-model.md) for the model itself.

## Status

Design phase. No code yet. Architecture decisions are recorded in [docs/adrs/](docs/adrs/).

Dagger is built from its own backlog, in [backlog.toml](backlog.toml), expressed in dagger's domain model. Each node is a self-contained work order with acceptance criteria and the scenarios its specs must cover.

```
scripts/ready.py            what can be worked now
scripts/ready.py --all      what cannot, and why
scripts/ready.py --show 18  the full work order for one node
```

An agent works one node at a time from an empty context window: read the work order, read the documents it references, build it, leave `make lint && make test` green. Never claim a node whose assignee is a user.

## Documentation

- [Vision](docs/vision.md)
- [Core principles](docs/core-principles.md)
- [Domain model](docs/domain-model.md)
- [Architecture](docs/architecture.md)
- [Roadmap](docs/roadmap.md)
- [Architecture decision records](docs/adrs/)
