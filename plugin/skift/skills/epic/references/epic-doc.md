# The epic document

A mini-PRD. It settles a design, records what was rejected and why, and hands
the decomposition a set of numbered requirements its nodes can cite. One person
reads it; no agent does.

Its frontmatter is the notes system's, not skift's:

```yaml
---
date: 2026-09-12
type: epic
status: draft          # draft · filed · done
project: "[[skift]]"   # the note for the project, not the skift slug
tags: [skift, patch, files]
---
```

Then a title and **one paragraph** that says what changes, in the indicative.
Start it with "Mini-PRD." so the genre is unmistakable. If that paragraph needs
two, the epic is two epics.

## Sections

**Background.** What is true today and why it is wrong, with evidence a reader
can check: file paths, command output, counts from the transcripts, specific
failures. Not impressions. This is the section that earns the rest, and the one
most often written as atmosphere. End it with a bolded **Why now** that says what
gets worse by waiting — sequencing is a claim and needs an argument.

**Goals.** Numbered outcomes, not tasks. "Evals run only when their inputs
change", not "build an eval runner". A node implements a goal; a goal is what you
would check months later.

**Non-goals.** The ones a reader would otherwise assume are in. Each with the
reason it is out, because a non-goal without a reason gets relitigated in the
first node that brushes against it. This is where a rejected design belongs when
rejecting it is part of the epic's point.

**Requirements.** Grouped by area, numbered **R1, R2, …** so a node body can cite
`R7` and a reviewer can check it. Each is one testable sentence about the system,
not an instruction. Numbering is the payoff: it is how ten node bodies stay
consistent with one design.

**Design choices.** The heart. One bolded lead-in per decision, then the decision,
why, and **what was rejected and what made it wrong** — with the specific case
that kills it where there is one. A design choice with no rejected alternative is
a description, and it will be re-litigated by the first agent who disagrees.

**Spikes.** Questions that must be answered before the work can be sized, each
ending in a written finding. "None" is a good answer when the tree already
answered them — then say what you read, so a reader knows the questions were
asked rather than skipped.

**Acceptance criteria for the epic.** Five to eight statements that are true when
every node has landed. Observable from outside: a command, a count, a file. They
are not the union of the nodes' criteria; they are the thing the nodes were for.

**Open questions.** Real uncertainty, each with what would settle it. A question
that is actually a decision belongs in Design choices; a question that is
actually work belongs in a node.

**Later.** Work this epic deliberately does not do but makes possible. It keeps
the non-goals honest by giving the deferred thing a home.

**Work orders.** One line: how many ops, where they are, and that they are not
applied. Point at the patches README.

**Provenance.** What was read to draft this, and what the conversation weighed
and rejected. This is what lets a reader six months on tell a considered
omission from an oversight.

Add a domain table wherever one replaces a page of prose — a tier table, a
scoping table. They are the most re-read part of an epic.

## Skeleton

```markdown
---
date: YYYY-MM-DD
type: epic
status: draft
project: "[[<Project>]]"
tags: [...]
---

# <Title: the change, not the area>

Mini-PRD. <One paragraph: what changes and what that buys.>

## Background
<What is true today, with evidence. Paths, counts, commands.>
**Why now.** <What gets worse by waiting.>

## Goals
1. **<Outcome>.** <What it means.>

## Non-goals
- **<Thing a reader would assume is in>.** <Why it is out.>

## Requirements
### <Area>
- **R1** <One testable sentence.>

## Design choices
**<Decision, as a claim>.** <Why. What was rejected and what killed it.>

## Spikes
<Ordered, each ending in a written finding — or "None", and what was read.>

## Acceptance criteria for the epic
- <Observable from outside when every node has landed.>

## Open questions
- **<Question>.** <What would settle it.>

## Later
- <Deferred, and what it waits for.>

## Work orders
Decomposed as N ops under `<slug>/patches/`, not yet applied. The README there
has the shape and the sizing.

## Provenance
Drafted YYYY-MM-DD from <what was read>. The conversation weighed <alternatives>
and rejected each for the reasons under [[#Design choices]].
```

## The patches README

Beside the node files, not in the document. Four things:

- **The line at the top**: which project or projects, that frontmatter is the op
  and the body is the work order, and — stated plainly — that nothing has been
  applied.
- **Before applying**: what a person must know or do first. Which nodes edit the
  same files and are therefore sequenced; which are gates; what is installed;
  what the epic assumes exists. This is where a drafter's knowledge that did not
  fit in any one body goes.
- **Shape**: the dependency graph as an ASCII diagram, then the op count by kind
  and **which nodes are ready at the start**. A diagram with no ready node means
  the graph is wrong.
- **Sizing** and **What it proves**: the calibration the nodes were sized against,
  the ones that came out largest, any session cost the work itself will spend,
  and one paragraph on what is demonstrably true once it all lands.

## Failure modes

- **Background as atmosphere.** No paths, no counts; nothing a reader can check.
- **Goals that are tasks.** If a goal names a file, it is a node.
- **Unnumbered requirements.** Then nothing can cite them and the nodes drift.
- **Design choices with no rejected alternative.** Re-litigated within a week.
- **An epic that cites itself.** Agents cannot read it. If a decision matters to
  a node, a node has to write it into the repository first.
