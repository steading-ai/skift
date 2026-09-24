// Package server is the service face of the binary: the skift.v1 Connect
// handlers mounted on an HTTP server that listens on a unix socket and on
// any further [Transport] it is given.
//
// The package owns the transports and nothing else. Service implementations
// are injected, so the handlers stay unaware of where a request came from,
// and the store is never reached from here. One mux carries every handler,
// and every transport serves that mux, so a second way in cannot serve a
// different set. See docs/architecture.md and
// docs/adrs/002-connect-rpc-is-the-only-door.md.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"connectrpc.com/connect"

	"github.com/steading-ai/skift/gen/skift/v1/skiftv1connect"
	"github.com/steading-ai/skift/internal/codec"
	"github.com/steading-ai/skift/internal/server/interceptor"
)

// Defaults for the fields of Config that may be left zero.
const (
	DefaultSocketMode      = os.FileMode(0o660)
	DefaultShutdownTimeout = 30 * time.Second
	// healthTimeout bounds one database probe from the health endpoint.
	healthTimeout = 2 * time.Second
)

// HealthPath is the route of the health check, outside the Connect
// namespace so a plain HTTP probe can hit it.
const HealthPath = "/healthz"

// Config is everything the transport needs to know.
type Config struct {
	// SocketPath is where the unix socket is created. Its parent directory
	// is created when missing. A stale socket nobody listens on is
	// replaced; a live one is an error.
	SocketPath string
	// SocketMode is the permission set on the socket. Zero means
	// [DefaultSocketMode], which admits the owner and the group only.
	SocketMode os.FileMode
	// SocketGroup is the group the socket is owned by, as a name or a
	// numeric id. Empty keeps the process's group.
	SocketGroup string
	// ShutdownTimeout bounds the drain. Zero means [DefaultShutdownTimeout].
	ShutdownTimeout time.Duration
	// Logger receives the server's structured log. Nil means slog.Default.
	Logger *slog.Logger
	// Interceptors wrap every Connect handler, in order. Principal
	// resolution is the first of them; see [interceptor.NewPrincipal].
	// The health check is outside the Connect namespace and not wrapped.
	Interceptors []connect.Interceptor
	// Transports are the ways into the handlers besides the unix socket,
	// such as the tailnet. Each serves the same mux behind the same
	// interceptors; what differs is what its connections carry, which is
	// what its identity resolver reads.
	Transports []Transport
}

// Transport is a way into the handlers besides the unix socket, which is
// built in. The tailnet in internal/server/transport is one.
type Transport interface {
	// Name identifies the transport in the log.
	Name() string
	// Listen binds the transport. It returns once the listener accepts
	// connections.
	Listen() (net.Listener, error)
	// ConnContext is the http.Server ConnContext for connections accepted
	// from the listener: what every request on the connection inherits,
	// and what the transport's identity resolver reads.
	ConnContext(ctx context.Context, conn net.Conn) context.Context
	// Close releases the transport once its listener has been closed.
	Close() error
}

// Services are the implementations mounted on the server.
type Services struct {
	Graph  skiftv1connect.GraphServiceHandler
	Work   skiftv1connect.WorkServiceHandler
	Events skiftv1connect.EventServiceHandler
}

// Pinger answers whether the database is reachable. A pgxpool.Pool is one.
type Pinger interface {
	Ping(context.Context) error
}

// Server is the Connect handlers and the health check on one mux, served
// by one HTTP server per transport.
type Server struct {
	cfg Config
	log *slog.Logger
	db  Pinger
	mux *http.ServeMux
	// http is the unix socket's server; it is the first of servers.
	http *http.Server
	// servers and listeners are paired by index: the socket first, then
	// one per configured Transport.
	servers   []*http.Server
	listeners []net.Listener
	// draining closes when shutdown begins, before the listeners do.
	// Handlers that wait see it through [Draining].
	draining chan struct{}
}

// New builds a server. Nil services are mounted as unimplemented, which
// answers every call with a Connect Unimplemented error rather than a 404.
func New(cfg Config, svc Services, db Pinger) *Server {
	if cfg.SocketMode == 0 {
		cfg.SocketMode = DefaultSocketMode
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = DefaultShutdownTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if svc.Graph == nil {
		svc.Graph = skiftv1connect.UnimplementedGraphServiceHandler{}
	}
	if svc.Work == nil {
		svc.Work = skiftv1connect.UnimplementedWorkServiceHandler{}
	}
	if svc.Events == nil {
		svc.Events = skiftv1connect.UnimplementedEventServiceHandler{}
	}

	s := &Server{
		cfg:      cfg,
		log:      cfg.Logger,
		db:       db,
		mux:      http.NewServeMux(),
		draining: make(chan struct{}),
	}
	// Every handler speaks skift's JSON, not protojson's: the one door
	// says what every other surface says. See internal/codec.
	opts := append([]connect.HandlerOption{connect.WithInterceptors(cfg.Interceptors...)}, codec.HandlerOptions()...)
	s.mux.Handle(skiftv1connect.NewGraphServiceHandler(svc.Graph, opts...))
	s.mux.Handle(skiftv1connect.NewWorkServiceHandler(svc.Work, opts...))
	s.mux.Handle(skiftv1connect.NewEventServiceHandler(svc.Events, opts...))
	s.mux.HandleFunc("GET "+HealthPath, s.health)

	// Every connection over the socket carries its peer's unix
	// credentials, which is what the unix identity resolver reads.
	s.http = s.newHTTPServer(interceptor.UnixConnContext)
	s.servers = append(s.servers, s.http)
	for _, t := range cfg.Transports {
		s.servers = append(s.servers, s.newHTTPServer(t.ConnContext))
	}
	return s
}

// newHTTPServer is one HTTP server over the shared mux. Transports differ
// only in what their connections carry.
func (s *Server) newHTTPServer(connContext func(context.Context, net.Conn) context.Context) *http.Server {
	return &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		// Every request context carries the drain signal.
		BaseContext: func(net.Listener) context.Context {
			return withDraining(context.Background(), s.draining)
		},
		ConnContext: connContext,
	}
}

// Listen binds the socket and applies its permissions, then binds each
// transport. It returns once every listener accepts connections, so a
// caller that needs to know the server is up waits on this rather than
// polling the path. A transport that fails to bind unwinds the ones before
// it.
func (s *Server) Listen() error {
	if len(s.listeners) > 0 {
		return errors.New("server: already listening")
	}
	l, err := listenUnix(s.cfg)
	if err != nil {
		return err
	}
	listeners := []net.Listener{l}
	s.log.Info("listening", "socket", s.cfg.SocketPath, "mode", fmt.Sprintf("%#o", s.cfg.SocketMode))
	for i, t := range s.cfg.Transports {
		tl, err := t.Listen()
		if err != nil {
			err = fmt.Errorf("server: %s: %w", t.Name(), err)
			for j := len(listeners) - 1; j >= 0; j-- {
				err = errors.Join(err, listeners[j].Close())
			}
			for j := i - 1; j >= 0; j-- {
				err = errors.Join(err, s.cfg.Transports[j].Close())
			}
			return err
		}
		listeners = append(listeners, tl)
		s.log.Info("listening", "transport", t.Name(), "addr", tl.Addr().String())
	}
	s.listeners = listeners
	return nil
}

// Serve accepts connections on every listener until ctx is cancelled, then
// drains: waiting handlers are told to return through [Draining], the
// listeners close, and in-flight requests finish, bounded by the shutdown
// timeout. The socket file is removed and the transports closed on the way
// out. Serve returns nil after a clean drain.
func (s *Server) Serve(ctx context.Context) error {
	if len(s.listeners) == 0 {
		return errors.New("server: Serve called before Listen")
	}
	defer func() {
		if err := os.Remove(s.cfg.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("remove socket", "socket", s.cfg.SocketPath, "err", err)
		}
		for _, t := range s.cfg.Transports {
			if err := t.Close(); err != nil {
				s.log.Warn("close transport", "transport", t.Name(), "err", err)
			}
		}
	}()

	// served receives one result per server; the first is enough to know
	// a listener failed while the rest keep running.
	served := make(chan error, len(s.servers))
	for i, srv := range s.servers {
		go func() {
			served <- srv.Serve(s.listeners[i])
		}()
	}

	select {
	case err := <-served:
		// Only a listener failure lands here; Shutdown has not run.
		return fmt.Errorf("server: serve: %w", errors.Join(err, s.closeAll()))
	case <-ctx.Done():
	}

	s.log.Info("draining", "timeout", s.cfg.ShutdownTimeout)
	close(s.draining)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()
	var shutdownErr error
	for _, srv := range s.servers {
		shutdownErr = errors.Join(shutdownErr, srv.Shutdown(shutdownCtx))
	}
	if shutdownErr != nil {
		s.log.Error("drain timed out; closing connections", "err", shutdownErr)
		return fmt.Errorf("server: shutdown: %w", errors.Join(shutdownErr, s.closeAll()))
	}
	for range s.servers {
		if err := <-served; !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("server: serve: %w", err)
		}
	}
	s.log.Info("stopped")
	return nil
}

// closeAll closes every server at once, cutting connections off.
func (s *Server) closeAll() error {
	var err error
	for _, srv := range s.servers {
		err = errors.Join(err, srv.Close())
	}
	return err
}

type drainingKey struct{}

func withDraining(ctx context.Context, ch <-chan struct{}) context.Context {
	return context.WithValue(ctx, drainingKey{}, ch)
}

// Draining returns a channel that closes when the server handling ctx's
// request starts shutting down. A handler that waits, such as ClaimNext with
// a wait, selects on it and returns cleanly, so the drain completes instead
// of the client being cut off. Outside a request the channel never closes.
func Draining(ctx context.Context) <-chan struct{} {
	if ch, ok := ctx.Value(drainingKey{}).(<-chan struct{}); ok {
		return ch
	}
	return nil
}
