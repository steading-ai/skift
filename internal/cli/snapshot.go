package cli

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/steading-ai/skift/internal/admin"
	"github.com/steading-ai/skift/internal/logging"
)

type snapshotOptions struct {
	databaseURL string
	logLevel    string
	dir         string
	keep        int
	ifBehind    int64
}

// newSnapshotCommand copies the database to a file before an upgrade
// migrates it. The rpm's pre-install step runs it with --if-behind. See
// docs/adrs/018-unattended-upgrades.md.
func newSnapshotCommand() *cobra.Command {
	opts := snapshotOptions{}
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Copy every table of the database to a snapshot file",
		Long: `Copy every table of skift's database, in one consistent transaction, to
a file in --dir labelled with the schema version it was taken at, and
print the file's path. The service may keep serving while it runs; writes
made meanwhile are not in the snapshot.

The file appears under its name only once it is complete and synced, so a
snapshot that fails leaves none. After a snapshot is in place, all but the
--keep newest in --dir are removed; a failed snapshot removes nothing.

With --if-behind VERSION, it first reads the database's schema version
and, when that is already VERSION or later, takes no snapshot, says why,
and exits 0. An upgrade runs it this way with the schema the new release
migrates to, so one that migrates nothing does nothing extra. A database
skift has never migrated has nothing to lose, so --if-behind skips it
too; without --if-behind, a snapshot of it fails.

The database's schema need not be this build's: the snapshot records
whatever version it holds.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if opts.keep < 1 {
				return usage("skift admin snapshot: --keep must be at least 1, not %d", opts.keep)
			}
			if opts.ifBehind < 0 {
				return usage("skift admin snapshot: --if-behind must be a schema version, not %d", opts.ifBehind)
			}
			return runSnapshot(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.dir, "dir", admin.DefaultSnapshotDir(), "directory to write the snapshot to ($STATE_DIRECTORY/backups when set)")
	f.IntVar(&opts.keep, "keep", admin.DefaultSnapshotKeep, "how many snapshots to keep in --dir, this one included")
	f.Int64Var(&opts.ifBehind, "if-behind", 0, "take a snapshot only if the database's schema version is below this one")
	f.StringVar(&opts.databaseURL, "database-url", os.Getenv(databaseURLEnv), "PostgreSQL connection URL (default: $"+databaseURLEnv+")")
	f.StringVar(&opts.logLevel, "log-level", "info", "minimum log level: debug, info, warn, error")
	return cmd
}

func runSnapshot(cmd *cobra.Command, opts snapshotOptions) error {
	var level slog.LevelVar
	if err := level.UnmarshalText([]byte(opts.logLevel)); err != nil {
		return fmt.Errorf("skift admin snapshot: --log-level: %w", err)
	}
	log := logging.New(cmd.ErrOrStderr(), &level)
	if opts.databaseURL == "" {
		return fmt.Errorf("skift admin snapshot: a database URL is required; pass --database-url or set %s", databaseURLEnv)
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	out, err := admin.TakeSnapshot(ctx, opts.databaseURL, admin.SnapshotParams{Dir: opts.dir, Keep: opts.keep, IfBehind: opts.ifBehind})
	if err != nil {
		return fmt.Errorf("skift admin snapshot: %w", err)
	}
	if out.Skipped != "" {
		log.Info("no snapshot taken: "+out.Skipped, "schema", out.SchemaVersion)
		return nil
	}
	for _, path := range out.Removed {
		log.Info("older snapshot removed", "path", path)
	}
	log.Info("snapshot taken", "path", out.Path, "schema", out.SchemaVersion)
	_, err = fmt.Fprintln(cmd.OutOrStdout(), out.Path)
	return err
}
