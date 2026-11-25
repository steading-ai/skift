package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/store"
)

// Job is one piece of skift's own background work, run on a schedule
// inside `skift serve`. See docs/adrs/016-background-jobs-and-admin.md.
type Job struct {
	// Name identifies the job: stable, lowercase, words joined by hyphens,
	// such as expire-leases. It is how an operator names the job.
	Name string
	// Every is the period between runs.
	Every time.Duration
	// Run does the work once. An error is recorded and the job retried on
	// River's schedule; the next period runs it again regardless.
	Run func(ctx context.Context) error
}

// ExpireLeases is the name of the job that releases every lease whose TTL
// has passed, so its node is ready again.
const ExpireLeases = "expire-leases"

// expireLeasesEvery is how often stale leases are expired. A lease outlives
// its TTL by at most this long.
const expireLeasesEvery = time.Minute

// Schedule is the background work `skift serve` runs. db is kept in shape
// directly, svc applies the graph's rules, and log records what each run
// did.
func Schedule(db *store.Store, svc *domain.Service, log *slog.Logger) []Job {
	return []Job{
		{
			Name:  ExpireLeases,
			Every: expireLeasesEvery,
			Run: func(ctx context.Context) error {
				reaped, err := svc.Reap(ctx, 0)
				for _, r := range reaped {
					for _, l := range r.Expired {
						log.Info("lease expired", "org_id", r.OrgID, "lease_id", l.ID, "node_id", l.NodeID, "holder", l.Holder, "expires_at", l.ExpiresAt)
					}
				}
				return err
			},
		},
		eventPartitionsJob(db, log),
	}
}

// scheduledArgs names the scheduled job one River job runs. Every
// scheduled job shares this kind and the one worker that dispatches on
// the name, so adding a job is adding a [Job], with nothing to register
// with River.
type scheduledArgs struct {
	Name string `json:"name"`
}

func (scheduledArgs) Kind() string { return "skift.scheduled" }

// scheduledWorker runs whichever scheduled job a River job names.
type scheduledWorker struct {
	river.WorkerDefaults[scheduledArgs]
	jobs map[string]Job
}

func (w *scheduledWorker) Work(ctx context.Context, job *river.Job[scheduledArgs]) error {
	scheduled, ok := w.jobs[job.Args.Name]
	if !ok {
		// A job a newer or older build scheduled, which this one does not
		// carry: cancel it rather than retry what can never run here.
		return river.JobCancel(fmt.Errorf("no scheduled job named %q", job.Args.Name))
	}
	return scheduled.Run(ctx)
}

// Jobs is the schedule running on River.
type Jobs struct {
	inner *store.Jobs
}

// StartJobs runs jobs on River over db until [Jobs.Stop]. River's tables
// must exist, so start it once the schema is current. Two jobs may not
// share a name.
func StartJobs(ctx context.Context, db *store.Store, log *slog.Logger, jobs []Job) (*Jobs, error) {
	byName := make(map[string]Job, len(jobs))
	periodic := make([]*river.PeriodicJob, 0, len(jobs))
	for _, job := range jobs {
		if job.Name == "" || job.Every <= 0 || job.Run == nil {
			return nil, fmt.Errorf("scheduled job %q needs a name, a positive period, and work to do", job.Name)
		}
		if _, dup := byName[job.Name]; dup {
			return nil, fmt.Errorf("two scheduled jobs are named %q", job.Name)
		}
		byName[job.Name] = job
		periodic = append(periodic, river.NewPeriodicJob(
			river.PeriodicInterval(job.Every),
			func() (river.JobArgs, *river.InsertOpts) {
				// Unique within the period: a leader handing over mid-period
				// cannot schedule the same run twice.
				return scheduledArgs{Name: job.Name}, &river.InsertOpts{
					UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: job.Every},
				}
			},
			&river.PeriodicJobOpts{ID: job.Name, RunOnStart: true},
		))
	}
	workers := river.NewWorkers()
	if err := river.AddWorkerSafely(workers, &scheduledWorker{jobs: byName}); err != nil {
		return nil, fmt.Errorf("register scheduled jobs: %w", err)
	}
	inner, err := db.NewJobs(store.JobsConfig{Workers: workers, Periodic: periodic, Logger: log})
	if err != nil {
		return nil, err
	}
	if err := inner.Start(ctx); err != nil {
		return nil, err
	}
	return &Jobs{inner: inner}, nil
}

// Stop stops the schedule, letting running jobs finish until ctx is done.
// A nil *Jobs, a schedule that never started, stops at once.
func (j *Jobs) Stop(ctx context.Context) error {
	if j == nil {
		return nil
	}
	err := j.inner.Stop(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
