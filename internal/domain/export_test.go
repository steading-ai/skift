package domain

import "time"

// WaitReachSQL is the recursive walk CheckAcyclic runs, for specs that
// measure how far it reaches.
const WaitReachSQL = waitReach

// LockNamespace is the first half of the advisory lock key Apply takes, for
// specs that hold the lock from outside.
const LockNamespace = lockNamespace

// NewULID mints an id the way events and nodes get theirs, for specs that
// check the ordering the event log relies on.
var NewULID = newULID

// LockNodeSQL is the statement a patch locks a node's row with before it
// reads the node's lease, for specs that hold a patch at that point.
const LockNodeSQL = lockNodeSQL

// Slugify and NormalizeSlug are how a project's slug is derived from its
// title or its requested slug, for specs that check the shape.
var (
	Slugify       = slugify
	NormalizeSlug = normalizeSlug
)

// AgeCursor re-issues cursor as if it had been taken at `at`, for specs
// that check the replay window without waiting for it.
func AgeCursor(cursor string, at time.Time) string {
	p, err := decodeCursor(cursor)
	if err != nil {
		panic(err)
	}
	p.At = at
	return encodeCursor(p)
}

// ReplayWindowStart is the earliest moment a cursor may point at.
var ReplayWindowStart = replayWindowStart

// ReadyFragments are the readiness conditions, each one subquery over the
// node n. ReadyWhere negates every one of them and DerivedSQL selects
// every one un-negated, which is what the structural spec asserts.
var ReadyFragments = map[string]string{
	"unsatisfied blockers": unsatisfiedBlockers,
	"live lease":           liveLease,
	"open children":        openChildren,
	"archived project":     archivedProject,
	"held ancestors":       heldAncestors,
	"held project":         heldProject,
}

// ReadyWhere and DerivedSQL are the two statements every fragment must
// appear in, for the spec that checks they cannot drift apart.
const (
	ReadyWhere = readyWhere
	DerivedSQL = derivedSQL
)

// Derive computes the derived flags of nodes, for specs that check the
// flags against the ready query they must agree with.
var Derive = derive
