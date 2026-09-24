// Package transport holds the ways into the Connect handlers beyond the unix
// socket the server package builds in. Each is a [server.Transport]: a
// listener, the context its connections carry, and the identity resolver
// that reads it. Handlers see none of this; see the request path in
// docs/architecture.md and docs/adrs/002-connect-rpc-is-the-only-door.md.
package transport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/tsnet"

	"github.com/steading-ai/skift/internal/server/interceptor"
)

// Defaults for the fields of TailnetConfig that may be left zero.
const (
	// DefaultTailnetPort is the tailnet port the handlers are served on.
	// Connect is plain HTTP here: WireGuard already encrypts and
	// authenticates the link.
	DefaultTailnetPort uint16 = 80
	// DefaultUpTimeout bounds how long Listen waits for the node to join
	// the tailnet before giving up.
	DefaultUpTimeout = 60 * time.Second
)

// TailnetConfig is what the service needs to be a node on a tailnet.
type TailnetConfig struct {
	// Hostname is the node's name on the tailnet. Required: it is how
	// clients find the service, and a node with the binary's name is a
	// node nobody meant to create.
	Hostname string
	// AuthKey enrols the node the first time it starts. Once the node's
	// state is on disk it is not needed. A tagged auth key produces a
	// tagged node, which is what a service should be.
	AuthKey string
	// Tags are the ACL tags the node advertises, each in the form
	// "tag:name". The auth key must permit them.
	Tags []string
	// StateDir is where the node keeps its keys and state across restarts.
	// Empty leaves tsnet to choose a directory under the user's config
	// directory named after the binary.
	StateDir string
	// Port is the tailnet port to listen on. Zero means
	// [DefaultTailnetPort].
	Port uint16
	// ControlURL is the coordination server. Empty means Tailscale's.
	ControlURL string
	// UpTimeout bounds how long Listen waits for the node to be running.
	// Zero means [DefaultUpTimeout].
	UpTimeout time.Duration
	// Logger receives tsnet's log. Nil means slog.Default. tsnet is
	// talkative; its own log lands at debug level and what it addresses to
	// the operator at info.
	Logger *slog.Logger
}

// Tailnet serves the handlers as a node on a tailnet, through tsnet, and
// identifies callers with WhoIs.
type Tailnet struct {
	cfg TailnetConfig
	log *slog.Logger
	ts  *tsnet.Server
	lc  *local.Client
}

// NewTailnet prepares a tailnet transport. Nothing touches the network until
// [Tailnet.Listen].
func NewTailnet(cfg TailnetConfig) (*Tailnet, error) {
	if cfg.Hostname == "" {
		return nil, errors.New("transport: tailnet hostname is required")
	}
	for _, tag := range cfg.Tags {
		if !strings.HasPrefix(tag, "tag:") || len(tag) == len("tag:") {
			return nil, fmt.Errorf("transport: tailnet tag %q must have the form tag:name", tag)
		}
	}
	if cfg.Port == 0 {
		cfg.Port = DefaultTailnetPort
	}
	if cfg.UpTimeout == 0 {
		cfg.UpTimeout = DefaultUpTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	log := cfg.Logger.With("transport", "tailnet")
	t := &Tailnet{cfg: cfg, log: log}
	t.ts = &tsnet.Server{
		Hostname:      cfg.Hostname,
		AuthKey:       cfg.AuthKey,
		AdvertiseTags: cfg.Tags,
		Dir:           cfg.StateDir,
		ControlURL:    cfg.ControlURL,
		Logf: func(format string, args ...any) {
			log.Debug(fmt.Sprintf(format, args...))
		},
		UserLogf: func(format string, args ...any) {
			log.Info(fmt.Sprintf(format, args...))
		},
	}
	return t, nil
}

// Name implements [server.Transport].
func (t *Tailnet) Name() string { return "tailnet" }

// Listen implements [server.Transport]: it brings the node up on the tailnet,
// bounded by UpTimeout, and binds the configured port. A node that has never
// been enrolled and has no auth key logs the login URL it needs and fails
// when the bound elapses.
func (t *Tailnet) Listen() (net.Listener, error) {
	ctx, cancel := context.WithTimeout(context.Background(), t.cfg.UpTimeout)
	defer cancel()
	status, err := t.ts.Up(ctx)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("transport: join tailnet as %q: %w", t.cfg.Hostname, err), t.ts.Close())
	}
	lc, err := t.ts.LocalClient()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("transport: tailnet local client: %w", err), t.ts.Close())
	}
	t.lc = lc
	l, err := t.ts.Listen("tcp", fmt.Sprintf(":%d", t.cfg.Port))
	if err != nil {
		return nil, errors.Join(fmt.Errorf("transport: listen on tailnet port %d: %w", t.cfg.Port, err), t.ts.Close())
	}
	ips := make([]string, 0, len(status.TailscaleIPs))
	for _, ip := range status.TailscaleIPs {
		ips = append(ips, ip.String())
	}
	t.log.Info("joined tailnet", "hostname", t.cfg.Hostname, "dns_name", status.Self.DNSName, "ips", ips, "port", t.cfg.Port)
	return l, nil
}

// ConnContext implements [server.Transport]. Every connection accepted from
// the tailnet is marked with its peer, which the tailscale resolver reads.
func (t *Tailnet) ConnContext(ctx context.Context, conn net.Conn) context.Context {
	return interceptor.TailnetConnContext(ctx, conn)
}

// Close implements [server.Transport]: it leaves the tailnet. The node's
// state stays on disk so the next start needs no auth key.
func (t *Tailnet) Close() error {
	if err := t.ts.Close(); err != nil {
		return fmt.Errorf("transport: close tailnet: %w", err)
	}
	return nil
}

// WhoIs implements [interceptor.WhoIs] over tsnet's local API, answering
// with the stable id of the node behind remoteAddr.
func (t *Tailnet) WhoIs(ctx context.Context, remoteAddr string) (string, error) {
	if t.lc == nil {
		return "", errors.New("transport: tailnet is not up")
	}
	res, err := t.lc.WhoIs(ctx, remoteAddr)
	if err != nil {
		return "", err
	}
	if res == nil || res.Node == nil {
		return "", errors.New("transport: WhoIs returned no node")
	}
	return string(res.Node.StableID), nil
}

// Resolver is the identity resolver for callers over this transport.
func (t *Tailnet) Resolver() interceptor.TailscaleResolver {
	return interceptor.TailscaleResolver{WhoIs: t}
}
