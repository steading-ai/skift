package cli

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/steading-ai/skift/internal/admin"
	"github.com/steading-ai/skift/internal/logging"
)

type restoreOptions struct {
	databaseURL string
	logLevel    string
	clean       bool
}

// newRestoreCommand brings a database back to a snapshot "skift admin
// snapshot" took. Only an operator runs it; nothing calls it on its own.
// See docs/adrs/018-unattended-upgrades.md.
func newRestoreCommand() *cobra.Command {
	opts := restoreOptions{}
	cmd := &cobra.Command{
		Use:   "restore <snapshot>",
		Short: "Bring the database back to exactly a snapshot",
		Long: `Replace the database's contents with the snapshot file that "skift admin
snapshot" wrote, and print what was restored as JSON. Every write made
since the snapshot was taken is lost. Stop skift serve first: restore warns,
but goes on, when other sessions are connected.

Restore rebuilds the schema the snapshot was taken at from the migrations
this build carries, then copies every table back in one transaction, checks
each table's row count against the snapshot, and sets every sequence. The
database ends at the snapshot's schema version, even when that is older
than this build's: start the skift release that serves that schema, or let
"skift admin migrate" bring it forward.

Restore refuses, before changing anything:
  - a snapshot file whose tables do not match its manifest;
  - a snapshot at a schema version this build does not carry;
  - a database whose schema already holds anything, unless --clean is
    given, which drops every table, view, sequence, function and type in
    the schema first. What --clean drops is not brought back if the
    restore then fails.

If migrating or copying fails, nothing of the snapshot is kept: restore
drops what it migrated and leaves the database empty.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRestore(cmd, opts, args[0])
		},
	}
	f := cmd.Flags()
	f.BoolVar(&opts.clean, "clean", false, "drop everything in the database's schema before restoring")
	f.StringVar(&opts.databaseURL, "database-url", os.Getenv(databaseURLEnv), "PostgreSQL connection URL (default: $"+databaseURLEnv+")")
	f.StringVar(&opts.logLevel, "log-level", "info", "minimum log level: debug, info, warn, error")
	return cmd
}

func runRestore(cmd *cobra.Command, opts restoreOptions, path string) error {
	var level slog.LevelVar
	if err := level.UnmarshalText([]byte(opts.logLevel)); err != nil {
		return fmt.Errorf("skift admin restore: --log-level: %w", err)
	}
	log := logging.New(cmd.ErrOrStderr(), &level)
	if opts.databaseURL == "" {
		return fmt.Errorf("skift admin restore: a database URL is required; pass --database-url or set %s", databaseURLEnv)
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	out, err := admin.Restore(ctx, opts.databaseURL, admin.RestoreParams{Path: path, Clean: opts.clean}, log)
	if err != nil {
		return fmt.Errorf("skift admin restore: %w", err)
	}
	log.Info("snapshot restored", "path", out.Path, "schema", out.SchemaVersion, "tables", len(out.Tables))
	raw, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("skift admin restore: %w", err)
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
	return err
}
