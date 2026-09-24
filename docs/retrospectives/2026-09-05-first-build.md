# Retrospective: the first build

**Date:** 2026-09-05
**Scope:** one design session and one autonomous run, from an empty directory to a working service, CLI, and MCP server.

Two voices. Ivy's is the product owner's; Claude's is the agent that did the design work, wrote the backlog, built the harness, and watched the run.

## What was built

Design: vision, principles, domain model, architecture, roadmap, seven ADRs, and a summary of the METR report that shaped the trust model. A 45-node backlog in skift's own domain model, each node a self-contained work order with acceptance criteria and Gherkin scenarios. A two-agent harness that drove it.

Result: 43 nodes in roughly 8.5 hours at concurrency one, zero review failures, $221.53 at list prices, 93M tokens of which 92% were cache reads. Verified by hand afterward: file a subtree in one patch, ready set, claim, complete under a fenced lease, dependents unblock, stale lease refused, parent ready when children close, search, event log, MCP. One design gap found by that hand test and fixed the same hour.

## Ivy's side

- Scoping skift to *finding ready work* found the seams of the software-factory architecture. Human-in-the-loop as a first-class mechanism, trust and provenance and principals nailed down, and the critical adjacent pieces (orchestrator, retrieval) carved out by deciding to broadcast to them rather than build one monolith.
- Reuse of hive and kb came from luck plus a lot of prior thought and experiments in this space.
- A bad assumption cost about nine months: "every beginner's project is a project management system," so this space was not worth exploring, even though it is a critical piece of the puzzle. The wait had one upside: beads, beans, Multica, and others all arrived at the same theory. DAGs.
- The bet is unproven. It stands until production says otherwise.
- The Hugging Face incident was happenstance. Digging into it coincided with this design, and the design is better for it.
- Asking ChatGPT for a suitable Ralph-loop harness, picking one of five, and handing it over to be repurposed worked in a couple of minutes.
- Gherkin was a big bet, held for a while: a balance humans and agents can both encode and read. The suite was easy to read for progress and expected behaviour. Full Cucumber would have been cumbersome; Ginkgo was the right balance. Still to be proven over time.
- Scope stayed honest. Not overbuilt, not reduced to a toy.
- 70% of the weekly token limit with 16 hours to reset, on a $200/month subscription that has rarely been maximized. A win.
- Connect-RPC, sqlc with goose and pgx, Ginkgo and Gomega, tsnet, and Go. The stack reads as over-engineering to peers and keeps proving itself: the API contract is easy to follow, the data store is tight and uses the database fully, and the suite is legible. testcontainers was new.

## Claude's side

### What worked, and why

- **The thesis held, for a specific reason.** Frontloaded design let agents work autonomously, but the mechanism was narrower than "good docs." Each work order gave the reviewer a mechanical contract: criteria to walk and scenarios that had to exist as named specs with matching assertions. Twenty-nine reviews were rigorous because the question was checkable, not because the reviewer was clever. Without that shape, reviewer judgment would have drifted by node ten.
- **Gherkin earned its place as the reviewer's contract first and as readable documentation second.** The readability was real, and it was the side effect. The cut that mattered was scenario-as-spec-name with no step-definition layer, which is the part of Cucumber agents would have fought.
- **The paper test found the cheapest bug of the session.** Writing skift's backlog in skift's model, before any schema, surfaced that a grouping node with children was "ready" under the rules as written. One rule added to readiness, zero code changed.
- **The domain model was the authority and the agents treated it as one.** Every migration header cites the section it implements. That is the document doing its job.
- **Broadcast, don't build.** ADR-006's responsibility table made "no" easy. Retrieval, notification, moderation, and supervision are consumers of the change stream, and skift works without any of them.
- **Outside review, taken by standing.** Twice, an external reviewer with no context was asked to look: once at the design, once at the skill. Both times the fact findings were excellent (write skew in cycle detection, lease fencing, a flag that did not exist) and the scope advice was a year-long plan. Take the facts, surface the intent, decide yourself. It worked both times.
- **Fresh-context agents are honest about the docs.** When node 3's work order named a deliverable inside a protected path, the implementer said the backlog was wrong and moved on. That is the escape hatch in the prompt doing what it was written for.

### What I would do differently

- **I broke my own run.** Committing to the working tree while a node was in flight made the harness think the agent had committed, soft-reset my work into the node's commit, and broke every push for twenty-four commits. Fixed with a force-push and a guard, and the rule is now in the skill: nobody commits during a run.
- **I trusted the suite over the vision.** The default policy described what a lease holder may do and never asked what an agent may do with no lease. Agents could not file work, the first thing the vision says they do. The acceptance suite could not catch it because I wrote its scenarios, and they filed as a user. The gap was mine twice. Ten minutes driving the CLI as the agent found what forty-three reviewed nodes had not. The lesson is in the skill: after a run, drive the system by hand as the principal the vision describes.
- **I underestimated the fixed cost per node.** Every agent spends about 1.8M tokens re-reading the design before writing a line. Granularity is not free. Knowing that, I would have merged several small nodes; forty-five was probably ten too many.
- **The contract was never grilled.** The human gate on the proto was removed so the loop could run overnight, and nodes 7 and 8 specified the contract in prose instead. The agents made the fine-grained choices and they landed well. That was a trade of a human decision for speed, and it happened to work. It might not have.
- **Concurrency one left most of the graph idle.** The DAG was five wide for most of the run. A worktree-per-node harness could plausibly have finished in under two hours. Safe was right for a first run and expensive as a default.
- **The decomposition was looser than I believed.** Three data-layer nodes became sixteen migrations, and one later node altered an earlier table. Nothing broke, but later nodes reached back into earlier ones in ways the dependency graph did not constrain. A generated end-state `schema.sql` would make that drift visible.
- **The reviewer's fail path is untested.** Zero failures across forty-three nodes means I have no evidence the loop recovers from a real rejection. The only rejections were harness gates. Before the next run: plant a deliberately incomplete implementation and confirm the reviewer catches it and the retry converges.

### Open bets

- **The model fits real work.** Skift's own backlog is still a TOML file. M1 is when skift holds it, and that is the first real evidence either way.
- **Provenance beats prohibition.** The trust model says a sanctioned, attributed, legible coordination channel is safer than pretending agents will not coordinate. The wikis incident is a third data point for the premise and no evidence yet for the conclusion.
- **Gherkin at scale.** It carried forty-five nodes. Whether it carries four hundred, across projects and principals, is unknown.

## Next run

1. Test the reviewer's fail path deliberately before trusting a zero-failure result.
2. Choose model and effort from the node's estimate, and calibrate from `harness/state.json`. Fable at medium was uniform and probably overkill for a third of the nodes.
3. Hold low-priority work until the last day of the plan window.
4. Worktree per node, concurrency above one.
5. `make schema` producing a committed end-state.
6. Move the backlog into skift. Retire the TOML file.
