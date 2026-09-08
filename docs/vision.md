# Vision

## The problem

Project management systems were built for humans and treat agents as, at best, a bot account. That fails the software lifecycle that is emerging, in which most implementation is done by agents and the human's job is intent, decomposition, and review.

The failure is concrete. GitHub Projects, used as a backlog for the [hive](https://github.com/ivy/hive) orchestrator, is slow, rate limited, reachable only through GraphQL, unreliable about reflecting the true board state, fragile under pagination, and searchable only through issues. It cannot express dependencies well enough to answer the one question an orchestrator needs answered: what is ready to work on right now?

Repo-local trackers answer that question but bind the backlog to one repository and one checkout. An organization's work spans repositories, machines, and agents.

## The thesis

A software project decomposes into a directed acyclic graph. Nodes are units of work. Edges are dependencies. A node whose blocking predecessors are all finished is ready, and every ready node can be worked in parallel by a different agent.

The graph is not static. An agent doing a spike learns that the plan was wrong and rewrites the subtree below it. An agent mid-task discovers a missing prerequisite and files it, blocking its own work until it lands. An agent that needs a credential, a button click, or a clarification files a node that only a human can complete. All of these are ordinary graph mutations, and the system must make them cheap, atomic, and safe.

## What dagger is

Dagger is the durable store for that graph and the API through which agents and humans build and mutate it. It answers four questions:

1. What exists, and how does it depend on what?
2. What is ready?
3. Who is working on what, and under what lease?
4. What did each piece of work produce, and what did it cost?

Agents are first-class principals. They create, decompose, link, claim, complete, and drop work through the same API humans use. Authorization policy decides what a given principal may do. The API is the domain model, so storage and transport can change underneath it.

Dagger is a product. It is proved out on a single home server with one to five users, and it is shaped to reach an organization with a thousand daily active users and hundreds of concurrent agents. Components are swapped as scale demands; contracts are not.

## Projects are free

The cost of exploration is falling, so bets get made more often and projects live for hours as readily as for years. A tracker that makes a project expensive to start turns the space into the team, the team into a dumping ground, and the only exit into declaring bankruptcy and archiving everything.

Creating a project in dagger takes no provisioning, no schema change, and no adjudication of a scarce name. Node numbers restart at 1 in each one, so a three-day project stays legible. Archiving reclaims nothing because nothing was scarce, and abandoned work is kept rather than tidied away: a failed spike is often the most useful record in the store.

Cheap projects trade a curation problem for a discovery problem. Hundreds of short-lived slugs mean search is how anything is found again, so lexical search arrives with the first dogfood milestone rather than being deferred indefinitely. Semantic retrieval and synthesized answers belong to a service consuming dagger's events, not to the store.

## What dagger is not

- **Not an orchestrator.** Dagger stores which work is live and exposes it with leases. Deciding which live work runs next, which agent, where, in what order, and against what budget, belongs to the orchestrator. What is live is intent, and intent is recorded here; what runs next is scheduling, and it is not. Hive and its successors are consumers.
- **Not an agent runtime.** Dagger never spawns, supervises, or talks to an agent. Agents and orchestrators talk to dagger.
- **Not a model of the organization.** Departments, teams, objectives, and company goals are not dagger's to represent. Group membership arrives as an input to policy from wherever the organization already keeps it, and a project management agent can work with a separate system for high-level priorities. The graph is work decomposition; conflating it with reporting lines or intent hierarchies is how trackers acquire their reputation.
- **Not a workflow engine.** There are no stages, columns, or state machines in dagger. A pipeline of define, plan, build, and review is four nodes and three edges. A gate that only a human can pass is a node assigned to a human.
- **Not a message bus.** Nodes, comments, and attachments are for describing and delivering work. Agent-to-agent coordination goes over A2A between the agents themselves.
- **Not a sandbox, a network boundary, or a misalignment detector.** Compute isolation, egress control, and supervision of agent behavior belong to the platform around dagger. Dagger makes coordination legible and attributed; it does not judge it.
- **Not a distributed filesystem.** Dagger stores immutable, content-addressed, size-capped blobs and references to artifacts that live elsewhere. It does not move large data between agents.

## The end state

A human describes what they want in conversation with an agent. The agent files the work as a graph. Orchestrators dispatch every ready node to an agent in parallel. Agents refine the graph as they learn. When a task needs a human, the human is notified and the rest of the graph keeps moving. The human comes back to results, reviews them, and answers the questions that were waiting.

A planning session can look at what completed work actually cost, in tokens by model, and estimate the rest. A budget owner can see what a subtree has consumed and what it is expected to consume.

## How to stay honest

- The core is nodes, edges, leases, holds, atomic patches, and the ready set. Everything else is a client or a column. If a feature needs more than that in the service, ask why.
- Fit for one server, shaped for a thousand. Tenancy columns exist from the first migration. Tenancy features do not exist until a second organization does.
- If `Ready` grows an opinion about which node should run *first*, an orchestrator's policy has leaked into the store. Leaving out work that someone recorded a decision to park is not that opinion.
- The proto contract is the design artifact. Spend design time there, not on tables.
- Build the minimum that lets one orchestrator run one node end to end. Then use it daily.
