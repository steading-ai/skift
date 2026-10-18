package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/logging"
	"github.com/steading-ai/skift/internal/policy"
	"github.com/steading-ai/skift/internal/sdnotify"
	"github.com/steading-ai/skift/internal/server"
	"github.com/steading-ai/skift/internal/server/interceptor"
	"github.com/steading-ai/skift/internal/server/transport"
	"github.com/steading-ai/skift/internal/version"
)

// databaseURLEnv is the variable the service reads its connection string
// from when --database-url is not given. .env.example documents it.
const databaseURLEnv = "DATABASE_URL"

// The tailnet is configured by environment as well as by flag, so that a
// packaged unit whose command line is fixed joins a tailnet from its
// EnvironmentFile alone. tailnetAuthKeyEnv is the name tailscaled itself
// uses, so one secret serves both.
const (
	tailnetHostnameEnv = "SKIFT_TAILNET_HOSTNAME"
	tailnetTagsEnv     = "SKIFT_TAILNET_TAGS"
	tailnetStateDirEnv = "SKIFT_TAILNET_STATE_DIR"
	tailnetAuthKeyEnv  = "TS_AUTHKEY"
)

// stateDirectoryEnv is what systemd sets under a StateDirectory=, colon
// separated when there are several. The node's state has to outlive a
// restart, or the service re-enrols as a new node, so under a unit it
// defaults to a directory there.
const stateDirectoryEnv = "STATE_DIRECTORY"

// defaultTailnetStateDir is $SKIFT_TAILNET_STATE_DIR, else "tailnet" under
// the unit's state directory, else empty, which leaves the choice to tsnet.
func defaultTailnetStateDir() string {
	if dir := os.Getenv(tailnetStateDirEnv); dir != "" {
		return dir
	}
	if dirs := os.Getenv(stateDirectoryEnv); dirs != "" {
		first, _, _ := strings.Cut(dirs, ":")
		return filepath.Join(first, "tailnet")
	}
	return ""
}

// defaultTailnetTags is $SKIFT_TAILNET_TAGS split on commas, blanks dropped.
func defaultTailnetTags() []string {
	var tags []string
	for tag := range strings.SplitSeq(os.Getenv(tailnetTagsEnv), ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags
}

// defaultSocketPath is the runtime directory of the invoking user when
// there is one, and the system runtime directory otherwise, which is where
// a systemd RuntimeDirectory= puts it.
func defaultSocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "skift", "skift.sock")
	}
	return "/run/skift/skift.sock"
}

type serveOptions struct {
	socket          string
	socketMode      string
	socketGroup     string
	databaseURL     string
	noMigrate       bool
	shutdownTimeout time.Duration
	logLevel        string
	tailnet         transport.TailnetConfig
}

func newServeCommand() *cobra.Command {
	opts := serveOptions{}
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the skift service",
		Long: `Run the skift service: the skift.v1 Connect handlers over PostgreSQL,
listening on a unix socket and, when --tailnet-hostname or
$SKIFT_TAILNET_HOSTNAME is given, as a node on a tailnet as well. Both serve
the same handlers; a caller over the socket is identified by its unix user,
over the tailnet by its tailscale node. Every --tailnet flag has an
environment variable, so a unit joins the tailnet from its EnvironmentFile.

The service brings the database to the current schema once it answers,
and reports itself ready only then, so a deployment needs no migration
step of its own. With --no-migrate it applies nothing and instead refuses
to serve a database that is behind the schema the binary carries; run
"skift migrate" first.

Logs go to stderr as structured records. Under systemd they carry journal
priorities, and under a Type=notify unit the service reports itself ready
once its socket is bound and the database has answered. SIGTERM and SIGINT
drain in-flight requests, including waiting ClaimNext calls, before the
process exits.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.socket, "socket", defaultSocketPath(), "path of the unix socket to listen on")
	f.StringVar(&opts.socketMode, "socket-mode", fmt.Sprintf("%04o", server.DefaultSocketMode), "octal permissions of the socket")
	f.StringVar(&opts.socketGroup, "socket-group", "", "group that owns the socket, by name or id (default: the process's group)")
	f.StringVar(&opts.databaseURL, "database-url", os.Getenv(databaseURLEnv), "PostgreSQL connection URL (default: $"+databaseURLEnv+")")
	f.BoolVar(&opts.noMigrate, "no-migrate", false, "apply no migrations at start, and refuse a database that is behind the schema")
	f.DurationVar(&opts.shutdownTimeout, "shutdown-timeout", server.DefaultShutdownTimeout, "how long to wait for in-flight requests on shutdown")
	f.StringVar(&opts.logLevel, "log-level", "info", "minimum log level: debug, info, warn, error")
	f.StringVar(&opts.tailnet.Hostname, "tailnet-hostname", os.Getenv(tailnetHostnameEnv), "also serve on the tailnet as a node with this name (default: $"+tailnetHostnameEnv+")")
	f.StringVar(&opts.tailnet.AuthKey, "tailnet-auth-key", os.Getenv(tailnetAuthKeyEnv), "auth key that enrols the tailnet node on first start (default: $"+tailnetAuthKeyEnv+")")
	f.StringSliceVar(&opts.tailnet.Tags, "tailnet-tag", defaultTailnetTags(), "ACL tag the tailnet node advertises, as tag:name; repeatable (default: $"+tailnetTagsEnv+", comma separated)")
	f.StringVar(&opts.tailnet.StateDir, "tailnet-state-dir", defaultTailnetStateDir(), "directory the tailnet node keeps its state in (default: $"+tailnetStateDirEnv+", else tailnet/ under the unit's $"+stateDirectoryEnv+", else tsnet's choice under the user config directory)")
	f.Uint16Var(&opts.tailnet.Port, "tailnet-port", transport.DefaultTailnetPort, "tailnet port to serve on")
	return cmd
}

// tailnetTransport builds the tailnet transport when one is configured, and
// returns nil when the service is to listen on the socket alone.
func tailnetTransport(opts serveOptions, log *slog.Logger) (*transport.Tailnet, error) {
	if opts.tailnet.Hostname == "" {
		if len(opts.tailnet.Tags) > 0 {
			return nil, errors.New("skift serve: --tailnet-tag needs --tailnet-hostname")
		}
		return nil, nil
	}
	cfg := opts.tailnet
	cfg.Logger = log
	tn, err := transport.NewTailnet(cfg)
	if err != nil {
		return nil, fmt.Errorf("skift serve: %w", err)
	}
	return tn, nil
}

func runServe(cmd *cobra.Command, opts serveOptions) error {
	var level slog.LevelVar
	if err := level.UnmarshalText([]byte(opts.logLevel)); err != nil {
		return fmt.Errorf("skift serve: --log-level: %w", err)
	}
	log := logging.New(cmd.ErrOrStderr(), &level)

	mode, err := strconv.ParseUint(opts.socketMode, 8, 32)
	if err != nil || mode > 0o777 {
		return fmt.Errorf("skift serve: --socket-mode %q is not an octal permission set", opts.socketMode)
	}
	if opts.databaseURL == "" {
		return fmt.Errorf("skift serve: a database URL is required; pass --database-url or set %s", databaseURLEnv)
	}
	tailnet, err := tailnetTransport(opts, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// The policy bundle compiles before anything listens: a service with
	// no policy would deny every request, which is a service not worth
	// starting.
	bundle, err := policy.LoadDefault(ctx)
	if err != nil {
		return fmt.Errorf("skift serve: %w", err)
	}

	// The pool connects lazily, so a database that is down at startup is
	// reported by the health check rather than preventing the service from
	// listening; systemd would otherwise restart it in a loop.
	pool, err := pgxpool.New(ctx, opts.databaseURL)
	if err != nil {
		return fmt.Errorf("skift serve: database url: %w", err)
	}
	defer pool.Close()

	// Every request is attributed to a principal before a handler sees it:
	// by unix peer credentials over the socket, by tailscale node over the
	// tailnet. The handlers reach the graph through the domain alone and
	// are the same set on both.
	svc := domain.NewService(pool, bundle)
	resolvers := []interceptor.Resolver{interceptor.UnixResolver{}}
	var transports []server.Transport
	if tailnet != nil {
		resolvers = append(resolvers, tailnet.Resolver())
		transports = append(transports, tailnet)
	}
	srv := server.New(server.Config{
		SocketPath:      opts.socket,
		SocketMode:      os.FileMode(mode),
		SocketGroup:     opts.socketGroup,
		ShutdownTimeout: opts.shutdownTimeout,
		Logger:          log,
		Interceptors: []connect.Interceptor{
			interceptor.NewPrincipal(server.NewIdentityLookup(svc), resolvers...),
		},
		Transports: transports,
	}, server.Services{
		Graph:  server.NewGraphService(svc),
		Work:   server.NewWorkService(svc),
		Events: server.NewEventService(svc),
	}, pool)

	log.Info("skift serve starting", "version", version.String(), "policy_version", bundle.Version())
	if err := srv.Listen(); err != nil {
		return fmt.Errorf("skift serve: %w", err)
	}
	// The shutdown notice is on the shutdown path itself, not in a
	// goroutine racing the return: the signal cancels serveCtx only once
	// the line is logged and systemd has been told, and Serve drains only
	// once serveCtx is cancelled. The order is therefore signal, log,
	// notify, drain, return, with nothing outstanding when runServe
	// returns.
	serveCtx, drain := context.WithCancel(context.WithoutCancel(ctx))
	defer drain()
	go func() {
		select {
		case <-ctx.Done():
			log.Info("shutdown signal received")
			notify(log, sdnotify.Stopping)
			drain()
		case <-serveCtx.Done():
			// A refusal below drained the service; there was no signal
			// to announce.
		}
	}()

	// The schema is prepared once the database answers, on the same
	// path that reports readiness. A database that refuses, by being
	// behind under --no-migrate or by failing a migration, is fatal:
	// the service drains and exits rather than serving over it.
	fatal := make(chan error, 1)
	go func() {
		if err := announceReady(ctx, pool, schemaPreparer(opts), log); err != nil {
			fatal <- err
			drain()
		}
	}()

	if err := srv.Serve(serveCtx); err != nil {
		return fmt.Errorf("skift serve: %w", err)
	}
	select {
	case err := <-fatal:
		return fmt.Errorf("skift serve: %w", err)
	default:
		return nil
	}
}

// schemaPreparer is what announceReady does to the schema once the
// database answers: apply the pending migrations, or under --no-migrate
// only check that there are none.
func schemaPreparer(opts serveOptions) func(ctx context.Context, log *slog.Logger) error {
	if opts.noMigrate {
		return func(ctx context.Context, log *slog.Logger) error {
			status, err := domain.InspectSchema(ctx, opts.databaseURL)
			if err != nil {
				return err
			}
			if status.Pending() {
				return fmt.Errorf("schema is at version %d but this build expects %d; run skift migrate, or start without --no-migrate", status.Current, status.Latest)
			}
			log.Info("schema current", "schema_version", status.Current)
			return nil
		}
	}
	return func(ctx context.Context, log *slog.Logger) error {
		report, err := domain.ApplyMigrations(ctx, opts.databaseURL)
		if err != nil {
			return err
		}
		for _, v := range report.Applied {
			log.Info("migration applied", "version", v)
		}
		log.Info("schema current", "applied", len(report.Applied), "schema_version", report.Version)
		return nil
	}
}

// readyProbe bounds one database ping while waiting to report readiness,
// and readyRetry is the pause between failed pings.
const (
	readyProbe = 2 * time.Second
	readyRetry = time.Second
)

// announceReady prepares the schema and reports the service ready to
// systemd once the database answers, and not before: the socket is
// already bound, but a service that cannot reach its database cannot take
// work, and a unit ordered after this one should not start until it can.
// It keeps probing until the database answers or the service is shutting
// down. The database answering and then refusing is returned as the error
// it refused with; that is the service's to act on, not to retry.
func announceReady(ctx context.Context, pool *pgxpool.Pool, prepare func(context.Context, *slog.Logger) error, log *slog.Logger) error {
	for {
		probeCtx, cancel := context.WithTimeout(ctx, readyProbe)
		err := pool.Ping(probeCtx)
		cancel()
		if err == nil {
			err = prepare(ctx, log)
			if err == nil {
				log.Info("database reachable; ready")
				notify(log, sdnotify.Ready, sdnotify.Status("serving"))
				return nil
			}
			if !errors.Is(err, domain.ErrDatabaseUnreachable) {
				log.Error("database refused; not serving", "err", err)
				return err
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		log.Warn("database not reachable; not ready yet", "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(readyRetry):
		}
	}
}

// notify sends states to systemd, logging a failure to reach the
// notification socket rather than failing the service over it.
func notify(log *slog.Logger, states ...string) {
	if _, err := sdnotify.Send(states...); err != nil {
		log.Warn("notify systemd", "err", err)
	}
}
