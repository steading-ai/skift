package cli

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/steading-ai/skift/internal/dbpool"
	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/logging"
	"github.com/steading-ai/skift/internal/policy"
	"github.com/steading-ai/skift/internal/version"
)

type reapOptions struct {
	databaseURL string
	logLevel    string
	pageSize    int
}

// reapSummary is what one run prints: how many leases it expired, per
// organization, so the journal shows at a glance whether the timer is
// doing anything.
type reapSummary struct {
	Expired       int              `json:"expired"`
	Organizations []reapOrgSummary `json:"organizations"`
}

type reapOrgSummary struct {
	OrgID   string   `json:"org_id"`
	Expired int      `json:"expired"`
	Leases  []string `json:"leases,omitempty"`
}

func newReapCommand() *cobra.Command {
	opts := reapOptions{}
	cmd := &cobra.Command{
		Use:   "reap",
		Short: "Expire stale leases",
		Long: `Release every lease whose TTL has passed, as expired, in every organization.
Each expiry is an event on the lease's node, and the node is ready again.

This is the service's own timer work, run by skift-reap.timer, and it
reaches the database directly rather than through the service. It is
idempotent, and two runs at once release each lease exactly once.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runReap(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.databaseURL, "database-url", os.Getenv(databaseURLEnv), "PostgreSQL connection URL (default: $"+databaseURLEnv+")")
	f.StringVar(&opts.logLevel, "log-level", "info", "minimum log level: debug, info, warn, error")
	f.IntVar(&opts.pageSize, "page-size", 0, "leases to expire per transaction (default: the page ceiling)")
	return cmd
}

func runReap(cmd *cobra.Command, opts reapOptions) error {
	var level slog.LevelVar
	if err := level.UnmarshalText([]byte(opts.logLevel)); err != nil {
		return fmt.Errorf("skift reap: --log-level: %w", err)
	}
	log := logging.New(cmd.ErrOrStderr(), &level)
	if opts.databaseURL == "" {
		return fmt.Errorf("skift reap: a database URL is required; pass --database-url or set %s", databaseURLEnv)
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	bundle, err := policy.LoadDefault(ctx)
	if err != nil {
		return fmt.Errorf("skift reap: %w", err)
	}
	pool, err := dbpool.New(ctx, opts.databaseURL)
	if err != nil {
		return fmt.Errorf("skift reap: database url: %w", err)
	}
	defer pool.Close()

	log.Debug("skift reap starting", "version", version.String(), "policy_version", bundle.Version())
	reaped, err := domain.NewService(pool, bundle).Reap(ctx, opts.pageSize)
	summary := reapSummary{Organizations: make([]reapOrgSummary, 0, len(reaped))}
	for _, r := range reaped {
		org := reapOrgSummary{OrgID: r.OrgID, Expired: len(r.Expired)}
		for _, l := range r.Expired {
			org.Leases = append(org.Leases, l.ID)
			log.Info("lease expired", "org_id", r.OrgID, "lease_id", l.ID, "node_id", l.NodeID, "holder", l.Holder, "expires_at", l.ExpiresAt)
		}
		summary.Expired += org.Expired
		summary.Organizations = append(summary.Organizations, org)
	}
	if err != nil {
		return fmt.Errorf("skift reap: %w", err)
	}
	log.Info("skift reap done", "expired", summary.Expired, "organizations", len(summary.Organizations))
	raw, err := json.Marshal(summary)
	if err != nil {
		return fmt.Errorf("skift reap: %w", err)
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
	return err
}
