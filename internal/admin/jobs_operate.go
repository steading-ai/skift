package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/riverqueue/river/rivertype"

	"github.com/steading-ai/skift/internal/store"
)

// An operator inspects and runs the schedule from outside `skift serve`,
// through an insert-only client: it reads and enqueues River's jobs but
// works none of them, which is serve's part.

// recentRuns bounds how far back [Operator.List] looks for each job's last run.
// Each job runs at most once per period, so this covers several periods of
// every job in the schedule.
const recentRuns = 500

// ErrNoWorker is what [Operator.Wait] reports when nothing picks the job up: no
// `skift serve` is running against the database. The job stays queued and
// runs once one is.
var ErrNoWorker = errors.New("nothing is working jobs: is skift serve running? The job stays queued and runs when it is")

// JobStatus is where one scheduled job stands.
type JobStatus struct {
	// Name is the job's name, as `skift admin jobs run` takes it.
	Name string `json:"name"`
	// Every is the period between runs, such as "1m0s".
	Every string `json:"every"`
	// LastRun is the newest run River recorded, if any.
	LastRun *JobRun `json:"last_run,omitempty"`
	// NextRunAt is when the next run is due, judging from when the last
	// one was scheduled. Absent before the first run.
	NextRunAt *time.Time `json:"next_run_at,omitempty"`
}

// JobRun is one run of a scheduled job.
type JobRun struct {
	// ID is River's id for the run.
	ID int64 `json:"id"`
	// Name is the scheduled job it ran.
	Name string `json:"name"`
	// Outcome is River's state for the run: available or scheduled while
	// queued, running, completed, retryable after a failed attempt that
	// will be retried, cancelled, or discarded once retries are spent.
	Outcome string `json:"outcome"`
	// At is when the run finished, or started if it has not.
	At *time.Time `json:"at,omitempty"`
	// Error is the last attempt's error, if it failed.
	Error string `json:"error,omitempty"`
}

// Succeeded reports whether the run completed.
func (r JobRun) Succeeded() bool { return r.Outcome == string(rivertype.JobStateCompleted) }

// Operator inspects and runs the schedule against one database, from
// outside `skift serve`.
type Operator struct {
	db       *store.Store
	schedule []Job
}

// NewOperator is an operator over db and the schedule serve runs.
func NewOperator(db *store.Store, schedule []Job) *Operator {
	return &Operator{db: db, schedule: schedule}
}

// List reports where each job in the schedule stands.
func (o *Operator) List(ctx context.Context) ([]JobStatus, error) {
	jobs, err := o.db.NewJobs(store.JobsConfig{})
	if err != nil {
		return nil, err
	}
	rows, err := jobs.Recent(ctx, scheduledArgs{}.Kind(), recentRuns)
	if err != nil {
		return nil, err
	}
	latest := map[string]*rivertype.JobRow{}
	for _, row := range rows {
		name, err := scheduledName(row)
		if err != nil {
			return nil, err
		}
		if _, seen := latest[name]; !seen {
			latest[name] = row
		}
	}
	statuses := make([]JobStatus, 0, len(o.schedule))
	for _, job := range o.schedule {
		status := JobStatus{Name: job.Name, Every: job.Every.String()}
		if row, ok := latest[job.Name]; ok {
			run := jobRun(job.Name, row)
			status.LastRun = &run
			next := row.ScheduledAt.Add(job.Every)
			status.NextRunAt = &next
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// Run queues one run of the named job now, for `skift serve` to work, and
// returns its id. A name the schedule does not carry is refused, naming
// those it does.
func (o *Operator) Run(ctx context.Context, name string) (int64, error) {
	names := make([]string, 0, len(o.schedule))
	for _, job := range o.schedule {
		names = append(names, job.Name)
	}
	if !slices.Contains(names, name) {
		return 0, fmt.Errorf("no scheduled job named %q; the schedule has %s", name, strings.Join(names, ", "))
	}
	jobs, err := o.db.NewJobs(store.JobsConfig{})
	if err != nil {
		return 0, err
	}
	return jobs.Enqueue(ctx, scheduledArgs{Name: name})
}

// Wait waits for the run with id to finish and reports how it went. A run
// that fails an attempt is reported then, rather than waited through its
// retries. If nothing has picked the run up within pickup, it returns the
// run as it stands and [ErrNoWorker].
func (o *Operator) Wait(ctx context.Context, id int64, pickup time.Duration) (JobRun, error) {
	jobs, err := o.db.NewJobs(store.JobsConfig{})
	if err != nil {
		return JobRun{}, err
	}
	deadline := time.Now().Add(pickup)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		row, err := jobs.Get(ctx, id)
		if err != nil {
			return JobRun{}, err
		}
		name, err := scheduledName(row)
		if err != nil {
			return JobRun{}, err
		}
		run := jobRun(name, row)
		switch row.State {
		case rivertype.JobStateCompleted, rivertype.JobStateCancelled, rivertype.JobStateDiscarded, rivertype.JobStateRetryable:
			return run, nil
		case rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateScheduled:
			if row.AttemptedAt == nil && time.Now().After(deadline) {
				return run, ErrNoWorker
			}
		case rivertype.JobStateRunning:
		}
		select {
		case <-ctx.Done():
			return run, ctx.Err()
		case <-tick.C:
		}
	}
}

// scheduledName is the scheduled job a River job row runs.
func scheduledName(row *rivertype.JobRow) (string, error) {
	var args scheduledArgs
	if err := json.Unmarshal(row.EncodedArgs, &args); err != nil {
		return "", fmt.Errorf("read job %d: %w", row.ID, err)
	}
	return args.Name, nil
}

func jobRun(name string, row *rivertype.JobRow) JobRun {
	run := JobRun{ID: row.ID, Name: name, Outcome: string(row.State)}
	switch {
	case row.FinalizedAt != nil:
		run.At = row.FinalizedAt
	case row.AttemptedAt != nil:
		run.At = row.AttemptedAt
	}
	if n := len(row.Errors); n > 0 && row.State != rivertype.JobStateCompleted {
		run.Error = row.Errors[n-1].Error
	}
	return run
}
