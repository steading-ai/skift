# Writing a node worth claiming

A node body is a **work order for an agent starting with an empty context
window**. It has none of your conversation, none of your reasoning, and no way
to ask you what you meant. Two sentences that are obvious while you write them
are ambiguous by the time anyone reads them.

This is the difference between a graph that can be worked unattended and a
backlog of reminders that only their author can act on.

## Structure

````markdown
## Context
What is true today, why this exists, and which documents govern it. Name the
files. If a decision was already made and written down, link it rather than
re-deciding.

## Deliverables
Concrete paths and artifacts, not intentions. "The health check in
roles/webdav/templates/webdav.container.j2", not "fix the health check".

## Acceptance criteria
- [ ] Testable statements, each with the command that tests it
- [ ] Written so a reviewer can check them without a judgement call

## Scenarios
```gherkin
Scenario: The behaviour that must hold
  Given some starting state
  When something happens
  Then the observable result
```

## Notes
The traps. What looks right and is not, what was already tried, which failure
this is a symptom of.
````

Not every node needs every section. A grouping node is a sentence. A node
tracking someone else's work has no deliverables of its own — give it context
and what unblocks here when it lands. But a node someone will **claim and
work** needs all of it.

## What each section is really for

**Context** is where the reader learns why this is not obvious. If the node
exists because something surprising happened, say what happened. The reader
cannot infer it and will otherwise repeat the mistake.

**Deliverables** are the diff. If you cannot name a path, the node is not ready
to be worked — it is a question, and should say so or be split.

**Acceptance criteria** are the contract, and the only thing a reviewer checks.
Prefer a criterion that names its own verification: not "the service is
healthy" but "`systemctl is-active foo` reports `active`, sustained across two
check intervals". Ambiguity here is where unattended work goes wrong: the agent
declares victory against its own reading.

**Scenarios** are for behaviour. Skip them for "delete this directory"; write
them wherever the interesting part is *what the system does*, and make the
`Then` clauses the assertions a test would make.

**Notes** carry the traps. This is the highest-value section per line, because
it is the only one that transfers hard-won knowledge rather than restating
intent. A trap that cost you an hour costs the next reader nothing.

## Priorities are advisory

Priority orders the ready set; it does not gate anything. Use `p0`/`p1` for
what genuinely should be taken next, and resist inflation — a backlog where
everything is `p1` orders by age, which is the same as having no priority.

## Gates

A node with `assignee_kind: user` is a **gate**: only a human completing it
satisfies it, and dropping it does not unblock what it blocks. That asymmetry
is deliberate — it is how an agent stops and asks for a decision without being
able to talk itself past the answer.

File a gate when the decision is genuinely a human's: an irreversible act, a
tradeoff with no technically correct answer, a policy. Then `release` your own
lease with reason `gate` and let the graph hold the state.

Do not file a gate to avoid deciding something you could decide. A gate that
should have been a judgement stalls the whole subtree behind it until someone
notices.

## Decomposition

Prefer one patch that builds the whole shape over a node you will "expand
later" — aliases exist so a decomposition is atomic, and a half-built graph is
worse than none.

Give a node children when the children are separately claimable work. Giving it
children removes it from the ready set until they close, which is what makes a
grouping node a milestone rather than a task.

Use `blocked_by` for genuine dependency: B cannot start until A is done.
Ordering preference is not dependency — encoding it as an edge serialises work
that could have run in parallel, and that cost is invisible until someone
wonders why only one node is ever ready.
