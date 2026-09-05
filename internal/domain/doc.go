// Package domain holds every rule that makes the graph a graph: cycle
// detection, readiness, leases, references. It is the only package that
// reaches the store, and every operation runs inside one store transaction.
// See docs/architecture.md and docs/domain-model.md.
package domain
