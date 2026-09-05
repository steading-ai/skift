package cli

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/ivy/dagger/internal/domain"
	"github.com/ivy/dagger/internal/logging"
	"github.com/ivy/dagger/internal/policy"
	"github.com/ivy/dagger/internal/server"
	"github.com/ivy/dagger/internal/server/interceptor"
	"github.com/ivy/dagger/internal/server/transport"
	"github.com/ivy/dagger/internal/version"
)

// databaseURLEnv is the variable the service reads its connection string
// from when --database-url is not given. .env.example documents it.
const databaseURLEnv = "DATABASE_URL"

// tailnetAuthKeyEnv is the variable the tailnet auth key is read from when
// --tailnet-auth-key is not given. It is the name tailscaled itself uses, so
// one secret serves both.
const tailnetAuthKeyEnv = "TS_AUTHKEY"

// defaultSocketPath is the runtime directory of the invoking user when
// there is one, and the system runtime directory otherwise, which is where
// a systemd RuntimeDirectory= puts it.
func defaultSocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "dagger", "dg.sock")
	}
	return "/run/dagger/dg.sock"
}

type serveOptions struct {
	socket          string
	socketMode      string
	socketGroup     string
	databaseURL     string
	shutdownTimeout time.Duration
	logLevel        string
	tailnet         transport.TailnetConfig
}

func newServeCommand() *cobra.Command {
	opts := serveOptions{}
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the dagger service",
		Long: `Run the dagger service: the dagger.v1 Connect handlers over PostgreSQL,
listening on a unix socket and, when --tailnet-hostname is given, as a node
on a tailnet as well. Both serve the same handlers; a caller over the socket
is identified by its unix user, over the tailnet by its tailscale node.

Logs go to stderr as structured records. Under systemd they carry journal
priorities. SIGTERM and SIGINT drain in-flight requests, including waiting
ClaimNext calls, before the process exits.`,
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
	f.DurationVar(&opts.shutdownTimeout, "shutdown-timeout", server.DefaultShutdownTimeout, "how long to wait for in-flight requests on shutdown")
	f.StringVar(&opts.logLevel, "log-level", "info", "minimum log level: debug, info, warn, error")
	f.StringVar(&opts.tailnet.Hostname, "tailnet-hostname", "", "also serve on the tailnet as a node with this name")
	f.StringVar(&opts.tailnet.AuthKey, "tailnet-auth-key", os.Getenv(tailnetAuthKeyEnv), "auth key that enrols the tailnet node on first start (default: $"+tailnetAuthKeyEnv+")")
	f.StringSliceVar(&opts.tailnet.Tags, "tailnet-tag", nil, "ACL tag the tailnet node advertises, as tag:name; repeatable")
	f.StringVar(&opts.tailnet.StateDir, "tailnet-state-dir", "", "directory the tailnet node keeps its state in (default: tsnet's choice under the user config directory)")
	f.Uint16Var(&opts.tailnet.Port, "tailnet-port", transport.DefaultTailnetPort, "tailnet port to serve on")
	return cmd
}

// tailnetTransport builds the tailnet transport when one is configured, and
// returns nil when the service is to listen on the socket alone.
func tailnetTransport(opts serveOptions, log *slog.Logger) (*transport.Tailnet, error) {
	if opts.tailnet.Hostname == "" {
		if len(opts.tailnet.Tags) > 0 {
			return nil, errors.New("dg serve: --tailnet-tag needs --tailnet-hostname")
		}
		return nil, nil
	}
	cfg := opts.tailnet
	cfg.Logger = log
	tn, err := transport.NewTailnet(cfg)
	if err != nil {
		return nil, fmt.Errorf("dg serve: %w", err)
	}
	return tn, nil
}

func runServe(cmd *cobra.Command, opts serveOptions) error {
	var level slog.LevelVar
	if err := level.UnmarshalText([]byte(opts.logLevel)); err != nil {
		return fmt.Errorf("dg serve: --log-level: %w", err)
	}
	log := logging.New(cmd.ErrOrStderr(), &level)

	mode, err := strconv.ParseUint(opts.socketMode, 8, 32)
	if err != nil || mode > 0o777 {
		return fmt.Errorf("dg serve: --socket-mode %q is not an octal permission set", opts.socketMode)
	}
	if opts.databaseURL == "" {
		return fmt.Errorf("dg serve: a database URL is required; pass --database-url or set %s", databaseURLEnv)
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
		return fmt.Errorf("dg serve: %w", err)
	}

	// The pool connects lazily, so a database that is down at startup is
	// reported by the health check rather than preventing the service from
	// listening; systemd would otherwise restart it in a loop.
	pool, err := pgxpool.New(ctx, opts.databaseURL)
	if err != nil {
		return fmt.Errorf("dg serve: database url: %w", err)
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
		Graph: server.NewGraphService(svc),
		Work:  server.NewWorkService(svc),
	}, pool)

	log.Info("dg serve starting", "version", version.String(), "policy_version", bundle.Version())
	if err := srv.Listen(); err != nil {
		return fmt.Errorf("dg serve: %w", err)
	}
	go func() {
		<-ctx.Done()
		log.Info("shutdown signal received")
	}()
	if err := srv.Serve(ctx); err != nil {
		return fmt.Errorf("dg serve: %w", err)
	}
	return nil
}
