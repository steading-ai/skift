package domain

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
