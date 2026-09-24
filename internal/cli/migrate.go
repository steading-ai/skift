package cli

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/logging"
	"github.com/steading-ai/skift/internal/version"
)

type migrateOptions struct {
	databaseURL string
	logLevel    string
}

func newMigrateCommand() *cobra.Command {
	opts := migrateOptions{}
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Bring the database to the current schema",
		Long: `Apply every migration the binary carries that the database has not seen,
in order, and print what was applied. A database already at the current
schema applies nothing, and that is not an error, so this is safe to run
on every deploy.

"skift serve" does the same at start unless told not to; this command is for
running the migrations ahead of the service, as an installer or a person.
Like "skift reap", it reaches the database directly rather than through the
service.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMigrate(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.databaseURL, "database-url", os.Getenv(databaseURLEnv), "PostgreSQL connection URL (default: $"+databaseURLEnv+")")
	f.StringVar(&opts.logLevel, "log-level", "info", "minimum log level: debug, info, warn, error")
	return cmd
}

func runMigrate(cmd *cobra.Command, opts migrateOptions) error {
	var level slog.LevelVar
	if err := level.UnmarshalText([]byte(opts.logLevel)); err != nil {
		return fmt.Errorf("skift migrate: --log-level: %w", err)
	}
	log := logging.New(cmd.ErrOrStderr(), &level)
	if opts.databaseURL == "" {
		return fmt.Errorf("skift migrate: a database URL is required; pass --database-url or set %s", databaseURLEnv)
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	log.Debug("skift migrate starting", "version", version.String())
	report, err := domain.ApplyMigrations(ctx, opts.databaseURL)
	if err != nil {
		return fmt.Errorf("skift migrate: %w", err)
	}
	for _, v := range report.Applied {
		log.Info("migration applied", "version", v)
	}
	log.Info("skift migrate done", "applied", len(report.Applied), "schema_version", report.Version)
	raw, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("skift migrate: %w", err)
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
	return err
}
