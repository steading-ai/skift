package cli

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/steading-ai/skift/internal/dbpool"
	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/logging"
	"github.com/steading-ai/skift/internal/version"
)

// newAdminCommand groups the operations on a deployment that no caller of
// the service can perform, because they are what makes there be a caller
// at all. Each reaches the database directly, as skift reap does.
func newAdminCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Administer a deployment directly against its database",
		Long: `Operations on a deployment that go around the service: the writes that
have to happen before the service has anyone to serve. Each takes the
database URL that "skift serve" takes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newBootstrapCommand())
	return cmd
}

type bootstrapOptions struct {
	databaseURL   string
	logLevel      string
	tailscaleNode string
}

func newBootstrapCommand() *cobra.Command {
	opts := bootstrapOptions{}
	cmd := &cobra.Command{
		Use:   "bootstrap <uid> <organization>",
		Short: "Create the first organization and its first principal",
		Long: `Create an organization with the given name and, in it, a user principal
that a caller on the unix socket with the given numeric uid resolves to.
Print both.

The command is idempotent. An organization already carrying the name is
adopted rather than duplicated, and so is a principal already holding the
uid, so an installer can run it on every provision without knowing whether
it has run before. The database must be at the current schema: run
"skift migrate" first.

The uid is the one "skift" will run as: for the invoking user, "$(id -u)".

With --tailscale-node, the same principal is also what a caller from that
tailnet node resolves to, when the service is serving the tailnet. The
value is the node's stable id, "tailscale status --json | jq -r .Self.ID"
on the node itself. A node already bound to the principal is adopted; one
bound to any other principal is refused.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			uid, err := strconv.ParseUint(args[0], 10, 32)
			if err != nil {
				return usage("skift admin bootstrap: uid %q is not a numeric user id", args[0])
			}
			if args[1] == "" {
				return usage("skift admin bootstrap: the organization name must not be empty")
			}
			return runBootstrap(cmd, opts, domain.BootstrapParams{UID: uint32(uid), OrgName: args[1], TailscaleNode: opts.tailscaleNode})
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.tailscaleNode, "tailscale-node", "", "stable id of a tailnet node the same principal calls from")
	f.StringVar(&opts.databaseURL, "database-url", os.Getenv(databaseURLEnv), "PostgreSQL connection URL (default: $"+databaseURLEnv+")")
	f.StringVar(&opts.logLevel, "log-level", "info", "minimum log level: debug, info, warn, error")
	return cmd
}

func runBootstrap(cmd *cobra.Command, opts bootstrapOptions, params domain.BootstrapParams) error {
	var level slog.LevelVar
	if err := level.UnmarshalText([]byte(opts.logLevel)); err != nil {
		return fmt.Errorf("skift admin bootstrap: --log-level: %w", err)
	}
	log := logging.New(cmd.ErrOrStderr(), &level)
	if opts.databaseURL == "" {
		return fmt.Errorf("skift admin bootstrap: a database URL is required; pass --database-url or set %s", databaseURLEnv)
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	pool, err := dbpool.New(ctx, opts.databaseURL)
	if err != nil {
		return fmt.Errorf("skift admin bootstrap: database url: %w", err)
	}
	defer pool.Close()

	log.Debug("skift admin bootstrap starting", "version", version.String())
	out, err := domain.Bootstrap(ctx, pool, params)
	if err != nil {
		return fmt.Errorf("skift admin bootstrap: %w", err)
	}
	log.Info("organization "+createdOrFound(out.Organization.Created), "org_id", out.Organization.ID, "name", out.Organization.Name)
	log.Info("principal "+createdOrFound(out.Principal.Created), "org_id", out.Organization.ID, "principal_id", out.Principal.ID, "kind", out.Principal.Kind)
	for _, id := range out.Principal.Identities {
		log.Info("identity "+createdOrFound(id.Created), "principal_id", out.Principal.ID, "identity", id.Scheme+":"+id.Value)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("skift admin bootstrap: %w", err)
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
	return err
}

func createdOrFound(created bool) string {
	if created {
		return "created"
	}
	return "found"
}
