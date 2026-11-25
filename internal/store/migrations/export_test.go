package migrations

import "time"

// LockID is the session advisory lock a migrating process holds.
const LockID = lockID

// LockTimeouts is how many lock timeouts the last Apply retried after.
func (m *Migrator) LockTimeouts() int { return m.lockTimeouts }

// SetRetryBudget shortens the retry budget, so a spec that holds a lock
// for good finds out quickly that Apply gives up.
func (m *Migrator) SetRetryBudget(d time.Duration) { m.retryBudget = d }
