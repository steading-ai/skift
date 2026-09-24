package domain

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// The wake signal of the change stream. Migration 00016 raises a
// notification on the events channel at every commit that wrote an
// event, carrying the organization it happened in and nothing else. A
// stream that hears one reads the events table from its cursor; the
// notification never carries an event, so a dropped one costs latency
// and never a loss. See the Change stream section of docs/domain-model.md.

// eventsChannel is the NOTIFY channel migration 00020 raises on.
const eventsChannel = "skift_events"

// A listening session that breaks reconnects with this backoff.
const (
	listenBackoffInitial = 100 * time.Millisecond
	listenBackoffMax     = 5 * time.Second
)

// listener is one LISTEN session shared by every stream a Service serves.
// It holds a connection of its own outside the pool, opened when the
// first stream subscribes and closed when the last leaves, and fans each
// notification out to the subscribers of its organization.
type listener struct {
	connect func(ctx context.Context) (*pgx.Conn, error)
	log     *slog.Logger

	mu      sync.Mutex
	subs    map[*subscription]struct{}
	session *session
}

// subscription is one stream's ear on the channel. wake carries one
// pending signal; a signal raised while one is pending coalesces with it,
// which is right for a consumer that reads everything on each wake.
type subscription struct {
	l     *listener
	orgID string
	wake  chan struct{}
}

// session is one run of the listening goroutine. ready closes once the
// first LISTEN has been executed, with err set when it could not be.
type session struct {
	ready  chan struct{}
	err    error
	cancel context.CancelFunc
	done   chan struct{}
}

func newListener(connect func(ctx context.Context) (*pgx.Conn, error)) *listener {
	return &listener{connect: connect, log: slog.Default(), subs: map[*subscription]struct{}{}}
}

// subscribe registers an ear for orgID and returns once the channel is
// being listened on, so a durable read made after it cannot miss a commit
// the listener would have announced. It fails when the listener cannot be
// established.
func (l *listener) subscribe(ctx context.Context, orgID string) (*subscription, error) {
	sub := &subscription{l: l, orgID: orgID, wake: make(chan struct{}, 1)}
	l.mu.Lock()
	l.subs[sub] = struct{}{}
	if l.session == nil {
		l.session = l.start()
	}
	s := l.session
	l.mu.Unlock()

	select {
	case <-s.ready:
	case <-ctx.Done():
		sub.close()
		return nil, ctx.Err()
	}
	if s.err != nil {
		sub.close()
		return nil, s.err
	}
	return sub, nil
}

// close withdraws the subscription. The last one out ends the session.
func (sub *subscription) close() {
	l := sub.l
	l.mu.Lock()
	delete(l.subs, sub)
	var s *session
	if len(l.subs) == 0 && l.session != nil {
		s, l.session = l.session, nil
	}
	l.mu.Unlock()
	if s != nil {
		s.cancel()
		<-s.done
	}
}

func (l *listener) start() *session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{ready: make(chan struct{}), cancel: cancel, done: make(chan struct{})}
	go l.run(ctx, s)
	return s
}

// run keeps the channel listened on until the session is cancelled. The
// first LISTEN failing ends the session, and every subscriber waiting on
// it, with the error; a later loss is logged and reconnected, and every
// subscriber is woken once the channel is listened on again, since a
// commit during the gap was not heard.
func (l *listener) run(ctx context.Context, s *session) {
	defer close(s.done)
	err := l.listen(ctx, func() { close(s.ready) })
	if err != nil && ctx.Err() == nil {
		s.err = err
		close(s.ready)
		l.mu.Lock()
		if l.session == s {
			l.session = nil
		}
		l.mu.Unlock()
		return
	}
	backoff := listenBackoffInitial
	for ctx.Err() == nil {
		l.log.Warn("event listener lost; reconnecting", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, listenBackoffMax)
		err = l.listen(ctx, l.wakeAll)
	}
}

// listen holds one connection on the channel, calling listening once
// LISTEN has executed, and returns why the connection ended.
func (l *listener) listen(ctx context.Context, listening func()) error {
	conn, err := l.connect(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+eventsChannel); err != nil {
		return fmt.Errorf("listen on %s: %w", eventsChannel, err)
	}
	listening()
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		l.wakeOrg(n.Payload)
	}
}

func (l *listener) wakeOrg(orgID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for sub := range l.subs {
		if sub.orgID == orgID {
			sub.signal()
		}
	}
}

func (l *listener) wakeAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for sub := range l.subs {
		sub.signal()
	}
}

func (sub *subscription) signal() {
	select {
	case sub.wake <- struct{}{}:
	default:
	}
}
