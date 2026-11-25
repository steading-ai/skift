// Package store is the data layer: sqlc-generated queries over the schema in
// migrations, plus the [Store] that owns the connection pool and the
// transactions those queries run in.
//
// Only the domain layer and the spec harness import this package, and only
// this package reaches PostgreSQL (depguard enforces both). Every domain
// operation runs inside one [Store.WithTx] call, so a partial
// write never becomes visible. Each query carries its organization predicate
// in static SQL; nothing a caller passes can widen it. See
// docs/adrs/006-provenance-not-authority.md.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/steading-ai/skift/internal/store/dbpool"
)

// DefaultStatementTimeout is how long one statement may run before the
// server cancels it, until [Store.SetStatementTimeout] changes it. It is
// a fixed structural ceiling rather than a tunable: see the Limits section
// of docs/domain-model.md.
const DefaultStatementTimeout = 30 * time.Second

// Store wraps a pgx pool and hands out transactions.
type Store struct {
	pool             *pgxpool.Pool
	statementTimeout time.Duration
}

// FromPool wraps a pool the caller owns, and closes.
func FromPool(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, statementTimeout: DefaultStatementTimeout}
}

// Open opens a pool on databaseURL and wraps it. The store owns that pool:
// the caller closes it with [Store.Close]. The pool connects lazily, so a
// database that is down is reported by [Store.Ping], not here.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := dbpool.New(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	return FromPool(pool), nil
}

// Close closes the pool, waiting for every connection to be returned.
func (s *Store) Close() {
	s.pool.Close()
}

// Ping reports whether the database answers.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// SetStatementTimeout sets how long one statement may run in any
// transaction the store opens from now on. Zero or less disables the
// timeout; a pathological query then runs until the client gives up.
func (s *Store) SetStatementTimeout(d time.Duration) {
	s.statementTimeout = d
}

// StatementTimeout is the timeout every transaction runs under.
func (s *Store) StatementTimeout() time.Duration {
	return s.statementTimeout
}

// Pool exposes the underlying pool, for specs that lay fixtures or watch
// connections from outside a transaction. Code outside the store never
// calls it: forbidigo rejects it outside _test.go files. See
// docs/adrs/013-postgresql-only-through-the-store.md.
func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

// Queries returns the generated query set bound to the pool, for reads that
// need no transaction.
func (s *Store) Queries() *Queries {
	return New(s.pool)
}

// Tx is what a transactional operation receives: the generated queries bound
// to the transaction, and the store's hand-written statements as methods.
// The transaction itself stays inside the store, so a caller cannot send it
// SQL of its own. See docs/adrs/013-postgresql-only-through-the-store.md.
type Tx struct {
	*Queries
	tx pgx.Tx
}

// Raw is the transaction under tx, for specs that write fixtures no query
// covers, such as backdating a row. Code outside the store never calls it:
// forbidigo rejects it outside _test.go files.
func Raw(tx Tx) pgx.Tx {
	return tx.tx
}

// WithTx runs fn in one transaction. A nil return commits; an error or a
// panic rolls back and propagates. Operations are read committed, the
// isolation level every domain operation is written against, with the
// advisory lock on edge-mutating patches doing the rest: see the
// Concurrency section of docs/domain-model.md.
//
// Every statement in the transaction runs under [Store.StatementTimeout],
// set locally to the transaction so a pooled connection carries nothing
// over. A statement that outlives it fails with SQLSTATE 57014 and the
// transaction rolls back.
func (s *Store) WithTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) (err error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if r := recover(); r != nil {
			_ = tx.Rollback(ctx)
			panic(r)
		}
		if err != nil {
			if rerr := tx.Rollback(ctx); rerr != nil && !errors.Is(rerr, pgx.ErrTxClosed) {
				err = errors.Join(err, fmt.Errorf("rollback: %w", rerr))
			}
		}
	}()
	if s.statementTimeout > 0 {
		// SET takes no bind parameters; the value is an integer of
		// milliseconds, never client text.
		if _, err = tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", s.statementTimeout.Milliseconds())); err != nil {
			return fmt.Errorf("set statement timeout: %w", err)
		}
	}
	if err = fn(ctx, Tx{Queries: New(tx), tx: tx}); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}
