package domain

import (
	"time"

	"github.com/steading-ai/skift/internal/store"
)

// ReadyHoldBlockers is the hold summary an empty first page of Ready
// carries, for the property that pins it to the ready set.
var ReadyHoldBlockers = readyHoldBlockers

// LockNamespace is the first half of the advisory lock key Apply takes, for
// specs that hold the lock from outside.
const LockNamespace = lockNamespace

// NewULID mints an id the way events and nodes get theirs, for specs that
// check the ordering the event log relies on.
var NewULID = newULID

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

// SeenAtHorizon re-issues cursor with one delivered event recorded at
// exactly its transaction boundary, the lowest a seen event may sit.
func SeenAtHorizon(cursor string) string {
	p, err := decodeCursor(cursor)
	if err != nil {
		panic(err)
	}
	p.Seen = append(p.Seen, seenEvent{TxID: p.XMin, ID: newULID()})
	return encodeCursor(p)
}

// Position and SeenEvent are the decoded cursor, for specs that check
// its arithmetic without a database.
type (
	Position  = position
	SeenEvent = seenEvent
)

// CursorAfter is the position a watcher holds once it has consumed the
// first n of a batch of events read at boundary xmin and time at, given
// the position it read from and its page size.
func CursorAfter(from Position, pageSize int, xmin int64, at time.Time, events []store.Event, n int) Position {
	w := &watcher{pageSize: pageSize, pos: from}
	return w.after(batch{xmin: xmin, at: at, events: events}, n)
}

// ReplayWindowStart is the earliest moment a cursor may point at.
var ReplayWindowStart = replayWindowStart

// Derive computes the derived flags of nodes, for specs that check the
// flags against the ready query they must agree with.
var Derive = derive

// NextProject is the round-robin rotation step, for specs that check its
// cursor arithmetic without a database.
var NextProject = nextProject
