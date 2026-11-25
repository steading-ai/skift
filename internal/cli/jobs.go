package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/steading-ai/skift/internal/admin"
	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/logging"
	"github.com/steading-ai/skift/internal/policy"
)

// jobPickup is how long `skift admin jobs run --wait` waits for a serve
// to pick the run up before saying nothing is working jobs.
const jobPickup = 15 * time.Second

type jobsOptions struct {
	databaseURL string
	logLevel    string
	wait        bool
}

// newJobsCommand groups inspecting and running skift's own background jobs,
// which `skift serve` runs on their schedule. See
// docs/adrs/016-background-jobs-and-admin.md.
func newJobsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "Inspect and run skift's background jobs",
		Long: `List skift's background jobs, which "skift serve" runs on their schedule,
or run one now. Running a job queues it for serve to work, so a manual run
takes the same path as a scheduled one.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newJobsListCommand(), newJobsRunCommand())
	return cmd
}

func jobsFlags(cmd *cobra.Command, opts *jobsOptions) {
	f := cmd.Flags()
	f.StringVar(&opts.databaseURL, "database-url", os.Getenv(databaseURLEnv), "PostgreSQL connection URL (default: $"+databaseURLEnv+")")
	f.StringVar(&opts.logLevel, "log-level", "info", "minimum log level: debug, info, warn, error")
}

func newJobsListCommand() *cobra.Command {
	opts := jobsOptions{}
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List each background job: its schedule, last run, and next run",
		Long: `Print each background job with its period, its last run and how that run
went, and when the next run is due.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withOperator(cmd, "skift admin jobs list", opts, func(ctx context.Context, op *admin.Operator) error {
				statuses, err := op.List(ctx)
				if err != nil {
					return err
				}
				return printJSON(cmd, statuses)
			})
		},
	}
	jobsFlags(cmd, &opts)
	return cmd
}

func newJobsRunCommand() *cobra.Command {
	opts := jobsOptions{}
	cmd := &cobra.Command{
		Use:   "run <job>",
		Short: "Run a background job now",
		Long: `Queue one run of the named job for "skift serve" to work, and print its id.

With --wait, block until the run finishes and print how it went, exiting
non-zero unless it completed. When no serve picks the run up, --wait says
so rather than hanging; the run stays queued and is worked once serve runs.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			const who = "skift admin jobs run"
			return withOperator(cmd, who, opts, func(ctx context.Context, op *admin.Operator) error {
				id, err := op.Run(ctx, args[0])
				if err != nil {
					return err
				}
				if !opts.wait {
					return printJSON(cmd, admin.JobRun{ID: id, Name: args[0], Outcome: "available"})
				}
				run, err := op.Wait(ctx, id, jobPickup)
				if printErr := printJSON(cmd, run); printErr != nil {
					return printErr
				}
				switch {
				case err != nil:
					return err
				case !run.Succeeded():
					return fmt.Errorf("job %s run %d ended %s: %s", run.Name, run.ID, run.Outcome, run.Error)
				}
				return nil
			})
		},
	}
	jobsFlags(cmd, &opts)
	cmd.Flags().BoolVar(&opts.wait, "wait", false, "wait for the run to finish and print how it went")
	return cmd
}

// withOperator opens the database and the schedule serve runs, and hands
// do an operator over both. Errors are prefixed with who.
func withOperator(cmd *cobra.Command, who string, opts jobsOptions, do func(ctx context.Context, op *admin.Operator) error) error {
	var level slog.LevelVar
	if err := level.UnmarshalText([]byte(opts.logLevel)); err != nil {
		return fmt.Errorf("%s: --log-level: %w", who, err)
	}
	log := logging.New(cmd.ErrOrStderr(), &level)
	if opts.databaseURL == "" {
		return fmt.Errorf("%s: a database URL is required; pass --database-url or set %s", who, databaseURLEnv)
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	bundle, err := policy.LoadDefault(ctx)
	if err != nil {
		return fmt.Errorf("%s: %w", who, err)
	}
	db, err := admin.Open(ctx, opts.databaseURL)
	if err != nil {
		return fmt.Errorf("%s: database url: %w", who, err)
	}
	defer db.Close()
	if err := do(ctx, admin.NewOperator(db, admin.Schedule(domain.NewService(db, bundle), log))); err != nil {
		return fmt.Errorf("%s: %w", who, err)
	}
	return nil
}

func printJSON(cmd *cobra.Command, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
	return err
}
