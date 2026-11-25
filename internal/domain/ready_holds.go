package domain

import (
	"context"

	"github.com/steading-ai/skift/internal/store"
)

// ReadyHoldBlocker is one hold affecting matching open work. Downstream
// includes parked descendants as well as blocked dependents, but never
// the held ticket itself.
type ReadyHoldBlocker struct {
	Reference            string
	Reason               string
	DownstreamCount      int64
	DownstreamReferences []string
}

const (
	readyHoldLimit       = 5
	readyDownstreamLimit = 3
)

// readyHoldBlockers ranks hold summaries by distinct downstream impact.
// The match filters select affected work, not sources or intermediate
// blockers: for example, a held user gate can prevent agent work.
func readyHoldBlockers(ctx context.Context, tx store.Tx, orgID string, q ReadyQuery, parentID string) ([]ReadyHoldBlocker, int64, error) {
	f, err := q.filter(parentID)
	if err != nil {
		return nil, 0, err
	}
	rows, total, err := tx.HoldBlockers(ctx, orgID, q.Projects, f, readyDownstreamLimit, readyHoldLimit)
	if err != nil {
		return nil, 0, err
	}
	blockers := make([]ReadyHoldBlocker, len(rows))
	for i, r := range rows {
		blockers[i] = ReadyHoldBlocker(r)
	}
	return blockers, total, nil
}
