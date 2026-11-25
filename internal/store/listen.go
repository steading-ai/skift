package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Listening is one LISTEN session on a notification channel. It holds a
// connection of its own, opened outside the pool on the pool's
// configuration, so a listener never holds a pooled connection idle.
type Listening struct {
	conn *pgx.Conn
}

// Listen opens a connection outside the pool and listens on channel. It
// returns once LISTEN has executed, so a read made after it cannot miss a
// notification raised by a later commit. On failure nothing is left open.
func (s *Store) Listen(ctx context.Context, channel string) (*Listening, error) {
	conn, err := pgx.ConnectConfig(ctx, s.pool.Config().ConnConfig)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	l := &Listening{conn: conn}
	if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
		l.Close()
		return nil, fmt.Errorf("listen on %s: %w", channel, err)
	}
	return l, nil
}

// Next blocks until a notification arrives and returns its payload, or
// returns why the session ended: ctx was done, or the connection was lost.
func (l *Listening) Next(ctx context.Context) (string, error) {
	n, err := l.conn.WaitForNotification(ctx)
	if err != nil {
		return "", err
	}
	return n.Payload, nil
}

// Close ends the session, giving the server a second to hear it go.
func (l *Listening) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = l.conn.Close(ctx)
}
