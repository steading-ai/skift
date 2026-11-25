---
status: accepted
date: 2026-11-25
---

# PostgreSQL is reached only through the store

## Context and Problem Statement

[ADR-007](007-core-tech-stack.md) chose sqlc for SQL and depguard so that only the domain layer imports the store. It never said where SQL may be written. `store.Tx` exports the raw pgx transaction "for statements the query set does not cover", and the domain layer has used it from the first readiness and cycle-detection work onward: eighteen production call sites, half of them static statements sqlc generates without difficulty, and a small query builder, `ReadyQuery.matchClauses`, that appends filters at runtime. Each new query an agent adds through the raw transaction is one more place a rule can be restated, and the empty-ready hold summary already restates hold inheritance and the wait relation beside the readiness fragments instead of composing them.

The documents also promised a portability the code never had. [Core principle 8](../core-principles.md#8-fit-for-one-server-shaped-for-a-thousand) said storage could be replaced behind an interface, and [the architecture](../architecture.md) said the domain owning the rules keeps another backend possible and the rules testable without a server. The rules are PostgreSQL: recursive CTEs, `FOR UPDATE SKIP LOCKED`, advisory locks, snapshot functions. There is no storage interface, and most domain specs need a database. Steading commits to PostgreSQL across its components and does not aim to be database-agnostic.

Where does SQL live, what may the domain layer do with the database, and how is that held when most of the code is written by agents?

## Decision Drivers

- Agents write most of the code, and a boundary that relies on review or on a stated rationale erodes one plausible exception at a time
- A rule written in several places drifts: the readiness conditions are composed into seven queries today, and one query already restates them
- PostgreSQL is a commitment, so its features are tools to use rather than portability risks to avoid
- Queries should be performant, expressive, easy to read, and stated once
- The proto contract is the domain model ([ADR-002](002-connect-rpc-is-the-only-door.md)); the schema stays an implementation detail behind it

## Considered Options

1. The store owns all SQL, enforced by the compiler and depguard
2. A lint rule on the raw transaction, with a stated reason for each exception
3. SQL stays in the domain layer, so the domain owns the rules
4. A storage interface, so a second backend stays possible
5. sqlc for every statement

## Decision Outcome

Chosen option: **the store owns all SQL, enforced by the compiler and depguard**, because it is the only option that makes the boundary something an agent cannot argue past.

**The boundary.** `internal/store` is the only package that talks to PostgreSQL: every SQL statement, every connection, and `LISTEN`. The domain layer keeps the rules as decisions: patch semantics, policy, fencing order, claim rotation, and the reasons Explain gives. It decides which question to ask and what to do with the answer, and never says how to ask it in SQL. What stays fixed across implementations is the Connect contract and tenancy, not the database.

**Enforcement is structural.** The raw transaction is unexported, so SQL written in the domain layer does not compile. depguard allows `github.com/jackc/pgx/v5` only inside `internal/store`, specs, and the spec harness in `internal/testsupport`. Specs that need raw SQL for fixtures reach it through `store.Raw` or the store's pool, which forbidigo rejects outside `_test.go` files. The composition root opens the database through the domain, so it names neither the store nor pgx. An agent that wants new SQL has to edit the store, where review sees it.

**PostgreSQL is used fully, and sqlc where it fits.** sqlc generates plain queries, and is the default. SQL that uses features sqlc cannot parse lives in `.sql` files inside the store, loaded with `go:embed`. There is no query builder.

**A rule is stated once, in the schema.** A condition that several queries share is defined once as a view or a SQL function, so the database rather than a string-matching spec guarantees that every query applies the same rule. The intended shape is a `wait_edges` view that unions `blocks` edges and containment, read by cycle detection, readiness, Explain and the hold summary; an `effective_holds(org)` function that walks down from the holds within one organization; and optional filters as nullable parameters planned per execution. A materialized view or a trigger-maintained flag is never used for this: either would store a derivation that can drift from the facts, which [principle 1](../core-principles.md#1-the-graph-is-the-truth) forbids.

**Move, then convert.** The SQL moves into the store first, byte for byte, so the existing specs and the readiness properties prove nothing changed. The rules then move into the schema one at a time, each measured with `EXPLAIN ANALYZE` against a graph at target scale and checked by the property suite. Until a rule is converted, its composed SQL lives in the store as Go constants.

### Consequences

- **Good**: A domain query is a compile error, not a review comment, and a pgx import outside the store fails lint.
- **Good**: Every statement is in one package, so a reader looking for how a rule is evaluated looks in one place.
- **Good**: Shared rules stated in the schema are applied identically by every query that reads them, which a test of string fragments only approximated.
- **Good**: The documents describe the system that exists. Principle 8 and the architecture no longer promise a portability nobody is building.
- **Bad**: The move is a large mechanical diff across the domain layer, and the store's API grows methods shaped like the domain's questions rather than like its tables.
- **Bad**: Once a rule lives in the schema, changing it is a migration. A view whose columns change needs a drop and re-create, and its current definition is whichever migration last replaced it.
- **Neutral**: sqlc stays, as the default for plain queries rather than the only way to write one.

## Rejected Options

### A lint rule on the raw transaction, with a stated reason for each exception

forbidigo can flag every use of the raw transaction, and `nolintlint` can require an explanation on each `nolint`.

- **Good**: Cheap to add, and every exception is visible in a diff.
- **Bad**: A linter sees Go syntax and cannot judge whether sqlc could express a query, so the rule cannot tell a needed exception from a convenient one.
- **Bad**: `nolintlint` checks that an explanation exists, not that it is true, and agents write plausible explanations for anything. The hold summary would have passed with an honest "filters assembled at runtime", and its actual problem, a restated rule, is invisible to a lint.

### SQL stays in the domain layer, so the domain owns the rules

The domain layer keeps writing composed SQL, because the readiness conditions are the domain's rules.

- **Good**: No move, and the rule's SQL sits next to the code that interprets its results.
- **Bad**: The portability that argument protects does not exist: the rules are already PostgreSQL SQL.
- **Bad**: It leaves SQL spread across eleven files, with nothing but convention stopping the twelfth.

### A storage interface, so a second backend stays possible

The domain depends on an interface, and PostgreSQL is one implementation of it.

- **Good**: A second backend becomes possible in principle.
- **Bad**: Steading commits to PostgreSQL, so there will be no second implementation, and an interface with one implementation is cost with no consumer. Principle 8 says nothing is built for scale until scale demands it.
- **Bad**: An interface general enough for another database gives up the PostgreSQL features the readiness and claim queries depend on.

### sqlc for every statement

Every query, including readiness and the filtered listings, is generated by sqlc.

- **Good**: One way to write a query, and generated types everywhere.
- **Bad**: sqlc cannot share a fragment between queries, so the readiness conditions would be restated in each one, which is the drift this decision exists to stop.
- **Bad**: Optional filters through `sqlc.narg` get generic plans on the claim path unless each execution is planned with its values, which sqlc does not control.

## More Information

This amends [ADR-007](007-core-tech-stack.md), which chose sqlc and depguard but not where SQL may live. Revisit the schema-stated rules if measurement shows a view or function cannot keep the plans the claim path depends on; the boundary itself does not depend on that answer.
