package admin

import (
	"context"
	"log/slog"
	"time"

	"github.com/steading-ai/skift/internal/store"
)

// EventPartitionsAhead is how many monthly events partitions past the
// current one are kept in place. Every write records an event, and an
// event with no partition for its month cannot be written, so the months
// ahead are the margin before writes would fail if the upkeep stopped:
// the job below runs daily, serve runs it at start, and migration 00028
// extends an upgraded database to the same horizon.
const EventPartitionsAhead = 3

// eventPartitionsEvery is how often the upkeep runs. Months turn slowly;
// daily leaves the whole horizon as slack.
const eventPartitionsEvery = 24 * time.Hour

// EnsureEventPartitions creates whichever events partitions are missing,
// from the month holding now to [EventPartitionsAhead] months after it,
// and logs the ones it created.
func EnsureEventPartitions(ctx context.Context, db *store.Store, log *slog.Logger, now time.Time) error {
	created, err := db.EnsureEventPartitions(ctx, now, EventPartitionsAhead)
	if err != nil {
		return err
	}
	if len(created) > 0 {
		log.Info("events partitions created", "partitions", created)
	}
	return nil
}

// eventPartitionsJob is the daily upkeep of the events partitions.
func eventPartitionsJob(db *store.Store, log *slog.Logger) Job {
	return Job{
		Name:  "ensure-event-partitions",
		Every: eventPartitionsEvery,
		Run: func(ctx context.Context) error {
			return EnsureEventPartitions(ctx, db, log, time.Now())
		},
	}
}
