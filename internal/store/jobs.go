package store

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// jobWorkers is how many jobs one process works at once. Skift's own
// background work is a handful of periodic chores, each quick, so a few
// are plenty; a long chore cannot starve the rest.
const jobWorkers = 4

// JobsConfig is what the admin package hands the store to run skift's own
// background work: the workers that do it, and the periodic jobs that
// schedule it. River's driver, and the pool it runs on, stay here. See
// docs/adrs/016-background-jobs-and-admin.md.
type JobsConfig struct {
	Workers  *river.Workers
	Periodic []*river.PeriodicJob
	Logger   *slog.Logger
}

// Jobs is skift's background work, running on River over the store's pool.
// Every process that starts it works jobs, and River elects one leader
// among them to schedule the periodic jobs, so each runs once per period
// however many processes run.
type Jobs struct {
	client *river.Client[pgx.Tx]
}

// NewJobs builds the River client on the store's pool. It schedules and
// works nothing until [Jobs.Start].
func (s *Store) NewJobs(cfg JobsConfig) (*Jobs, error) {
	client, err := river.NewClient(riverpgxv5.New(s.pool), &river.Config{
		Workers:      cfg.Workers,
		PeriodicJobs: cfg.Periodic,
		Queues:       map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: jobWorkers}},
		Logger:       cfg.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("build job client: %w", err)
	}
	return &Jobs{client: client}, nil
}

// Start begins working jobs and contending for leadership. River's own
// tables must exist: the schema migrations create them.
func (j *Jobs) Start(ctx context.Context) error {
	if err := j.client.Start(ctx); err != nil {
		return fmt.Errorf("start jobs: %w", err)
	}
	return nil
}

// Stop stops taking new jobs and waits for running ones to finish, until
// ctx is done; then it cancels them and returns.
func (j *Jobs) Stop(ctx context.Context) error {
	if err := j.client.Stop(ctx); err != nil {
		if stopErr := j.client.StopAndCancel(context.WithoutCancel(ctx)); stopErr != nil {
			return fmt.Errorf("stop jobs: %w", stopErr)
		}
	}
	return nil
}

// Enqueue inserts one job outside any transaction, for work started on
// demand rather than on its schedule.
func (j *Jobs) Enqueue(ctx context.Context, args river.JobArgs) error {
	if _, err := j.client.Insert(ctx, args, nil); err != nil {
		return fmt.Errorf("enqueue %s: %w", args.Kind(), err)
	}
	return nil
}
