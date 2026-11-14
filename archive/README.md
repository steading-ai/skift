# Archive

Machinery skift no longer runs and records written for one moment in its history, kept so the history stays readable.

Nothing here is maintained or authoritative. The code is not built, linted, tested or updated. The documents are not kept current, and a link or path inside one may name something that has since moved. Each file is as it was when it was moved here; they are records, so they are not edited.

The current design is in [docs/](../docs/), and [AGENTS.md](../AGENTS.md) says how to work in the repository.

| Was at | Is at | What it was |
|---|---|---|
| `harness/` | [harness/](harness/) | The reference agent loop that built the first version: a Python program that claimed one node at a time from a running skift service and ran an implementer agent and a reviewer agent on it. |
| `scripts/ready.py` | [scripts/ready.py](scripts/ready.py) | A paper test of the readiness rules: it computes the ready set of a flat-file backlog, from before there was a schema. |
| `backlog.toml` | [backlog.toml](backlog.toml) | Skift's first backlog: 45 nodes in skift's domain model, each a work order, in one TOML file. |
| `.claude/skills/ralph-loop/` | [claude-skills/ralph-loop/](claude-skills/ralph-loop/) | A Claude Code skill for writing work orders in `backlog.toml` and driving `harness/loop.py`. |
| `AGENT_REVIEW.md` | [AGENT_REVIEW.md](AGENT_REVIEW.md) | Notes from one agent session on 2026-09-06 that used skift as a client: where the interface caused friction and what it cost in tokens. |
| `docs/plans/` | [docs/plans/](docs/plans/) | Dated plans from September 2026: the sprint plan "prove the bet" and the implementation plans of two nodes. |
| `docs/research/` | [docs/research/](docs/research/) | One research note: a summary of METR and Redwood Research's report on the OpenAI / Hugging Face incident. |
| `docs/retrospectives/` | [docs/retrospectives/](docs/retrospectives/) | One retrospective, of the first build, dated 2026-09-05. |
