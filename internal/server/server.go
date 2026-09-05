// Package server is the service face of the binary: the dagger.v1 Connect
// handlers mounted on an HTTP server that listens on a unix socket.
//
// The package owns the transport and nothing else. Service implementations
// are injected, so the handlers stay unaware of where a request came from,
// and the store is never reached from here. See docs/architecture.md and
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

	"github.com/ivy/dagger/gen/dagger/v1/daggerv1connect"
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
}

// Services are the implementations mounted on the server.
type Services struct {
	Graph  daggerv1connect.GraphServiceHandler
	Work   daggerv1connect.WorkServiceHandler
	Events daggerv1connect.EventServiceHandler
}

// Pinger answers whether the database is reachable. A pgxpool.Pool is one.
type Pinger interface {
	Ping(context.Context) error
}

// Server is one HTTP server carrying the Connect handlers and the health
// check.
type Server struct {
	cfg      Config
	log      *slog.Logger
	db       Pinger
	mux      *http.ServeMux
	http     *http.Server
	listener net.Listener
	// draining closes when shutdown begins, before the listener does.
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
		svc.Graph = daggerv1connect.UnimplementedGraphServiceHandler{}
	}
	if svc.Work == nil {
		svc.Work = daggerv1connect.UnimplementedWorkServiceHandler{}
	}
	if svc.Events == nil {
		svc.Events = daggerv1connect.UnimplementedEventServiceHandler{}
	}

	s := &Server{
		cfg:      cfg,
		log:      cfg.Logger,
		db:       db,
		mux:      http.NewServeMux(),
		draining: make(chan struct{}),
	}
	s.mux.Handle(daggerv1connect.NewGraphServiceHandler(svc.Graph))
	s.mux.Handle(daggerv1connect.NewWorkServiceHandler(svc.Work))
	s.mux.Handle(daggerv1connect.NewEventServiceHandler(svc.Events))
	s.mux.HandleFunc("GET "+HealthPath, s.health)

	s.http = &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		// Every request context carries the drain signal.
		BaseContext: func(net.Listener) context.Context {
			return withDraining(context.Background(), s.draining)
		},
	}
	return s
}

// Handler is the mounted mux, for serving over some other transport.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// Listen binds the socket and applies its permissions. It returns once the
// socket accepts connections, so a caller that needs to know the server is
// up waits on this rather than polling the path.
func (s *Server) Listen() error {
	if s.listener != nil {
		return errors.New("server: already listening")
	}
	l, err := listenUnix(s.cfg)
	if err != nil {
		return err
	}
	s.listener = l
	s.log.Info("listening", "socket", s.cfg.SocketPath, "mode", fmt.Sprintf("%#o", s.cfg.SocketMode))
	return nil
}

// Serve accepts connections until ctx is cancelled, then drains: waiting
// handlers are told to return through [Draining], the listener closes, and
// in-flight requests finish, bounded by the shutdown timeout. The socket
// file is removed on the way out. Serve returns nil after a clean drain.
func (s *Server) Serve(ctx context.Context) error {
	if s.listener == nil {
		return errors.New("server: Serve called before Listen")
	}
	defer func() {
		if err := os.Remove(s.cfg.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("remove socket", "socket", s.cfg.SocketPath, "err", err)
		}
	}()

	served := make(chan error, 1)
	go func() {
		served <- s.http.Serve(s.listener)
	}()

	select {
	case err := <-served:
		// Only a listener failure lands here; Shutdown has not run.
		return fmt.Errorf("server: serve: %w", err)
	case <-ctx.Done():
	}

	s.log.Info("draining", "timeout", s.cfg.ShutdownTimeout)
	close(s.draining)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		s.log.Error("drain timed out; closing connections", "err", err)
		return fmt.Errorf("server: shutdown: %w", errors.Join(err, s.http.Close()))
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server: serve: %w", err)
	}
	s.log.Info("stopped")
	return nil
}

// ListenAndServe is [Server.Listen] followed by [Server.Serve].
func (s *Server) ListenAndServe(ctx context.Context) error {
	if err := s.Listen(); err != nil {
		return err
	}
	return s.Serve(ctx)
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
