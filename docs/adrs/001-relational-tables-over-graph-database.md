---
status: accepted
date: 2026-09-04
---

# Store the DAG in plain PostgreSQL tables, not a graph database

## Context and Problem Statement

Dagger's domain is a directed acyclic graph of tasks. Does a graph shaped domain need a graph database, or a graph extension on PostgreSQL, or is a relational schema sufficient at the target scale of a thousand daily active users and hundreds of concurrent agents?

## Decision Drivers

- The hot path is the ready set and claims, not traversal
- One shared PostgreSQL already exists in the platform zone; a second datastore is operational cost with no consumer
- Concurrency correctness for claims matters more than query expressiveness
- sqlc and goose are the chosen data tooling and both assume SQL

## Considered Options

1. Plain tables with recursive CTEs
2. PostgreSQL with the Apache AGE extension (Cypher)
3. A dedicated graph database such as Neo4j

## Decision Outcome

Chosen option: **plain tables with recursive CTEs**, because every query dagger needs is either a one-hop anti-join or a bounded recursive query over small, shallow components, and claims are the well-known job-queue pattern that PostgreSQL row locking handles directly.

### Consequences

- **Good**: Readiness is an anti-join on an indexed edge target, scoped by project. Microseconds.
- **Good**: `FOR UPDATE SKIP LOCKED` gives correct, contention-free claims under hundreds of concurrent orchestrators, as used by River, Oban, and graphile-worker.
- **Good**: Cycle detection and subtree rollups are recursive CTEs bounded by a connected component that is rarely deeper than 20 or wider than a few thousand nodes.
- **Good**: Transactions make derived views consistent under concurrent mutation, which is harder in most graph stores.
- **Bad**: Variable-length pattern matching across millions of edges would be slow. Dagger has no such query.
- **Neutral**: Every query must be tenant and project scoped so `Ready` never scans across organizations. That is required regardless of engine.
- **Neutral**: The event table is time-partitioned from the first migration.

## Rejected Options

### Apache AGE

- **Good**: Cypher on the same PostgreSQL.
- **Bad**: An extension to build, operate, and upgrade. Maturity is uneven. Poor fit with sqlc.

### Dedicated graph database

- **Good**: Best for whole-graph algorithms and deep pattern matching.
- **Bad**: Second datastore, second set of credentials, weaker transactional story with the rest of the data, no need at target scale.

## More Information

Revisit if a required query becomes a variable-length path search across a component larger than tens of thousands of nodes, or if SQL/PGQ lands in PostgreSQL and simplifies the recursive queries.
