package domain

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/steading-ai/skift/internal/policy"
	"github.com/steading-ai/skift/internal/store"
)

// The change stream: events of an organization after a cursor, in the
// order their transactions were assigned ids, filtered by project and
// operation, and policy-filtered so a consumer sees only what its
// principal may read. See the Change stream section of
// docs/domain-model.md.
//
// The cursor is not an event id. An id is a ULID minted before commit, so
// a transaction can take a low id, stall, and commit after a higher id
// has been delivered; a cursor that was the last id would skip it for
// good. A position is instead a transaction id boundary below which every
// transaction is known to have finished, taken from a snapshot's xmin,
// plus the events at or above the boundary that have already been
// delivered. Reading from a position delivers every visible event at or
// above the boundary that is not in that set, and a late commit lands at
// or above the boundary by construction, so it is delivered when it
// becomes visible. The set stays small: it is the events of transactions
// that were in flight when the position was taken.
//
// PostgreSQL LISTEN/NOTIFY is the wake signal and the events table is the
// transport: see wake.go. Delivery is at least once; a consumer that
// resumes from a cursor may see an event it was handed just before it
// stopped, and deduplicates by event id.

// Errors a stream is refused with.
var (
	// ErrCursorInvalid: the cursor is not one this service issued.
	ErrCursorInvalid = errors.New("malformed cursor")
	// ErrCursorExpired: the cursor is older than the replay window. The
	// consumer resynchronizes rather than resuming, since events after
	// it may no longer be offered.
	ErrCursorExpired = errors.New("cursor is older than the replay window")
)

// ReplayPartitions is how many monthly event partitions, the current one
// included, a consumer may resume within. Audit retention is separate
// and indefinite; this bounds only how far back a cursor may point. See
// principle 7 in docs/core-principles.md.
const ReplayPartitions = 2

const (
	// watchPollInterval is how long a stream waits for a wake signal
	// before reading again anyway. The read is the transport; the signal
	// only lowers latency, so a dropped one costs at most this.
	watchPollInterval = 30 * time.Second
	// cursorRefresh is how often a quiet stream is handed a fresh
	// position, so a consumer's stored cursor does not age out of the
	// replay window while nothing happens.
	cursorRefresh = time.Hour
	// cursorPrefix versions the cursor encoding.
	cursorPrefix = "v1."
)

// OpWatch is the action an event is authorized as before it is delivered.
// It is never written to events, since a read changes nothing.
const OpWatch = "watch"

// WatchRequest opens the change stream from a position.
type WatchRequest struct {
	// Cursor is where to resume, from an earlier [WatchEvent]. Empty
	// starts from now: events committed from this moment on.
	Cursor string
	// Project restricts the stream to one project, by id, slug, or
	// org/slug. Empty is every project of the organization.
	Project string
	// Ops restricts the stream to these operations. Empty is every
	// operation.
	Ops []string
	// PageSize bounds one read, at most [Limits.PageSize]. Zero takes
	// that ceiling.
	PageSize int
	// Stop ends the stream cleanly when it closes, as a draining server
	// asks; the consumer resumes from its cursor elsewhere.
	Stop <-chan struct{}
}

// WatchEvent is one message of the stream: an event and the cursor to
// resume after it, or a cursor alone when the position moved without an
// event to show. A cursor alone follows an interval the policy filtered
// entirely, opens a stream started from now, and refreshes a quiet one.
type WatchEvent struct {
	Event  *Event
	Cursor string
}

// position is the decoded cursor.
type position struct {
	// XMin is the transaction id boundary: every event of a transaction
	// below it has been delivered or will never be visible.
	XMin int64 `json:"x"`
	// Seen is every delivered event of a transaction at or above XMin.
	Seen []seenEvent `json:"s,omitempty"`
	// At is when the position was taken, for the replay window.
	At time.Time `json:"a"`
}

type seenEvent struct {
	TxID int64  `json:"t"`
	ID   string `json:"i"`
}

func encodeCursor(p position) string {
	raw, err := json.Marshal(p)
	if err != nil {
		panic(err) // a position is plain data; it always encodes
	}
	return cursorPrefix + base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(cursor string) (position, error) {
	var p position
	encoded, ok := strings.CutPrefix(cursor, cursorPrefix)
	if !ok {
		return p, fmt.Errorf("%w: %q", ErrCursorInvalid, cursor)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return p, fmt.Errorf("%w: %q", ErrCursorInvalid, cursor)
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.XMin < 0 || p.At.IsZero() {
		return p, fmt.Errorf("%w: %q", ErrCursorInvalid, cursor)
	}
	for _, s := range p.Seen {
		if s.TxID < p.XMin || s.ID == "" {
			return p, fmt.Errorf("%w: %q", ErrCursorInvalid, cursor)
		}
	}
	return p, nil
}

// replayWindowStart is the earliest moment a cursor may point at, and
// the earliest event the stream offers: the start of the oldest of the
// [ReplayPartitions] monthly partitions counting back from now.
func replayWindowStart(now time.Time) time.Time {
	t := now.UTC()
	month := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	return month.AddDate(0, -(ReplayPartitions - 1), 0)
}

// WatchEvents streams the organization's events after req.Cursor to
// send, for actor, until ctx ends, req.Stop closes, or send fails. It
// establishes the listener first, then reads what is durable, then
// waits, so a commit during setup is delivered by the read whether or
// not its notification was heard.
func (g *Service) WatchEvents(ctx context.Context, orgID string, actor Actor, req WatchRequest, send func(WatchEvent) error) error {
	if err := actor.validate(); err != nil {
		return err
	}
	pageSize, err := boundPageSize(req.PageSize, ActiveLimits())
	if err != nil {
		return err
	}
	var resume *position
	if req.Cursor != "" {
		p, err := decodeCursor(req.Cursor)
		if err != nil {
			return err
		}
		if p.At.Before(replayWindowStart(time.Now())) {
			return fmt.Errorf("%w: position taken %s", ErrCursorExpired, p.At.UTC().Format(time.RFC3339))
		}
		resume = &p
	}
	w := &watcher{svc: g, orgID: orgID, actor: actor, pageSize: pageSize, ops: append([]string{}, req.Ops...)}
	if req.Project != "" {
		err := g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			p, err := resolveProject(ctx, tx, orgID, req.Project)
			if err != nil {
				return err
			}
			w.projectID = &p.ID
			return nil
		})
		if err != nil {
			return err
		}
	}

	sub, err := g.wake.subscribe(ctx, orgID)
	if err != nil {
		return fmt.Errorf("listen for events: %w", err)
	}
	defer sub.close()

	if resume == nil {
		origin, err := w.origin(ctx)
		if err != nil {
			return err
		}
		w.pos = origin
		if err := send(WatchEvent{Cursor: encodeCursor(origin)}); err != nil {
			return err
		}
	} else {
		w.pos = *resume
	}

	for {
		if err := w.catchUp(ctx, send); err != nil {
			return err
		}
		timer := time.NewTimer(watchPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-req.Stop:
			timer.Stop()
			return nil
		case <-sub.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// watcher is one open stream.
type watcher struct {
	svc       *Service
	orgID     string
	actor     Actor
	projectID *string
	ops       []string
	pageSize  int
	pos       position
}

// batch is one read from a position: the rows in stream order, which of
// them the policy lets the actor see, and the boundary and moment the
// read was taken at.
type batch struct {
	xmin    int64
	at      time.Time
	events  []store.Event
	visible []bool
}

// origin is the position of now: the boundary of the current snapshot,
// with every event already visible at or above it counted as seen, so a
// transaction in flight at this moment is delivered when it commits and
// one that has committed is not.
func (w *watcher) origin(ctx context.Context) (position, error) {
	var p position
	err := w.svc.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		xmin, err := snapshotXMin(ctx, tx)
		if err != nil {
			return err
		}
		p = position{XMin: xmin, At: time.Now()}
		rows, err := tx.ListEventPositionsFrom(ctx, store.ListEventPositionsFromParams{
			OrgID: w.orgID, MinTxID: xmin, RetainedSince: replayWindowStart(p.At),
		})
		if err != nil {
			return fmt.Errorf("read event positions: %w", err)
		}
		for _, row := range rows {
			p.Seen = append(p.Seen, seenEvent{TxID: row.TxID, ID: row.ID})
		}
		return nil
	})
	return p, err
}

// catchUp reads and delivers everything after the current position until
// a read comes back short of a page.
func (w *watcher) catchUp(ctx context.Context, send func(WatchEvent) error) error {
	for {
		b, err := w.read(ctx)
		if err != nil {
			return err
		}
		delivered := false
		for i := range b.events {
			if !b.visible[i] {
				continue
			}
			delivered = true
			ev := b.events[i]
			if err := send(WatchEvent{Event: &ev, Cursor: encodeCursor(w.after(b, i+1))}); err != nil {
				return err
			}
		}
		next := w.after(b, len(b.events))
		// An interval the policy filtered entirely still moves the
		// position, and the consumer has to be told so its stored
		// cursor keeps up; so does a quiet stream, less often.
		refresh := len(b.events) == 0 && b.at.Sub(w.pos.At) >= cursorRefresh
		w.pos = next
		if (len(b.events) > 0 && !delivered) || refresh {
			if err := send(WatchEvent{Cursor: encodeCursor(next)}); err != nil {
				return err
			}
		}
		if len(b.events) < w.pageSize {
			return nil
		}
	}
}

// read is one page from the current position. The boundary is taken
// before the rows, in a statement of its own: every transaction below it
// had finished before the rows were read, so a row of one that is not
// visible will never be, and the boundary is safe to advance to.
func (w *watcher) read(ctx context.Context) (batch, error) {
	var b batch
	err := w.svc.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		xmin, err := snapshotXMin(ctx, tx)
		if err != nil {
			return err
		}
		b = batch{xmin: xmin, at: time.Now()}
		seen := make([]string, 0, len(w.pos.Seen))
		for _, s := range w.pos.Seen {
			seen = append(seen, s.ID)
		}
		b.events, err = tx.ListEventsFromPosition(ctx, store.ListEventsFromPositionParams{
			OrgID: w.orgID, MinTxID: w.pos.XMin, RetainedSince: replayWindowStart(b.at),
			Seen: seen, ProjectID: w.projectID, Ops: w.ops,
			PageSize: int32(w.pageSize), //nolint:gosec // bounded by the page ceiling
		})
		if err != nil {
			return fmt.Errorf("read events from position: %w", err)
		}
		r := &recorder{tx: tx, orgID: w.orgID, actor: w.actor, policy: w.svc.pol}
		b.visible = make([]bool, len(b.events))
		for i, ev := range b.events {
			if b.visible[i], err = r.mayWatch(ctx, ev); err != nil {
				return err
			}
		}
		return nil
	})
	return b, err
}

// after is the position once the first n rows of b have been consumed.
// The boundary advances to the read's, unless rows of the page may remain
// unconsumed, when it stops at the last consumed row's transaction so
// the rest of that transaction is read again. Seen keeps every consumed
// event at or above the new boundary and drops the rest.
func (w *watcher) after(b batch, n int) position {
	xmin := b.xmin
	if n > 0 && (n < len(b.events) || len(b.events) == w.pageSize) {
		xmin = min(xmin, b.events[n-1].TxID)
	}
	p := position{XMin: xmin, At: b.at}
	for _, s := range w.pos.Seen {
		if s.TxID >= xmin {
			p.Seen = append(p.Seen, s)
		}
	}
	for _, ev := range b.events[:n] {
		if ev.TxID >= xmin {
			p.Seen = append(p.Seen, seenEvent{TxID: ev.TxID, ID: ev.ID})
		}
	}
	return p
}

// snapshotXMin is the boundary of the statement's snapshot: the lowest
// transaction id still in progress, so every id below it has finished.
func snapshotXMin(ctx context.Context, tx store.Tx) (int64, error) {
	var xmin int64
	if err := tx.Tx.QueryRow(ctx, `SELECT pg_snapshot_xmin(pg_current_snapshot())::text::bigint`).Scan(&xmin); err != nil {
		return 0, fmt.Errorf("read snapshot boundary: %w", err)
	}
	return xmin, nil
}

// mayWatch asks the policy whether the actor may see ev. An event about
// a node carries the node as it stands; one above that scope carries no
// resource. A refusal is an answer, not an error; only a policy that
// could not decide is one.
func (r *recorder) mayWatch(ctx context.Context, ev store.Event) (bool, error) {
	in := policy.Input{
		Principal: policy.Principal{ID: r.actor.PrincipalID, Kind: r.actor.PrincipalKind},
		Action:    OpWatch,
	}
	if ev.NodeID != nil {
		node, err := r.tx.GetNode(ctx, store.GetNodeParams{OrgID: r.orgID, ID: *ev.NodeID})
		if err != nil {
			return false, fmt.Errorf("read node %s of event %s: %w", *ev.NodeID, ev.ID, err)
		}
		info, err := r.nodeInfo(ctx, node)
		if err != nil {
			return false, err
		}
		in.Resource = policy.Resource{Node: info}
	}
	err := r.policy.Authorize(ctx, in)
	var denied *policy.DeniedError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &denied) && denied.Err == nil:
		return false, nil
	default:
		return false, fmt.Errorf("authorize watch of event %s: %w", ev.ID, err)
	}
}
