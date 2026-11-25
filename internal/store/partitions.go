package store

import (
	"context"
	"fmt"
	"time"
)

// partitionLockTimeout bounds how long creating an events partition waits
// for its lock. Creating one briefly takes an ACCESS EXCLUSIVE lock on
// events, and a request queued behind a waiting lock waits too, so the
// work gives up quickly and is tried again on its next run instead.
const partitionLockTimeout = "2s"

// EnsureEventPartitions makes sure the events partition holding at exists,
// with ahead more monthly partitions after it, and returns the names of
// the ones it created: none when all were there already. See the
// ensure_event_partitions routine in migration 00007.
func (s *Store) EnsureEventPartitions(ctx context.Context, at time.Time, ahead int32) ([]string, error) {
	var created []string
	err := s.WithTx(ctx, func(ctx context.Context, tx Tx) error {
		if _, err := tx.tx.Exec(ctx, "SET LOCAL lock_timeout = '"+partitionLockTimeout+"'"); err != nil {
			return fmt.Errorf("bound the partition lock wait: %w", err)
		}
		var err error
		created, err = tx.EnsureEventPartitions(ctx, EnsureEventPartitionsParams{At: at, Ahead: ahead})
		if err != nil {
			return fmt.Errorf("ensure event partitions: %w", err)
		}
		return nil
	})
	return created, err
}
