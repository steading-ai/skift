package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"

	"connectrpc.com/connect"

	"github.com/ivy/dagger/gen/dagger/v1/daggerv1connect"
)

// Transport names how a client reaches the service.
type Transport string

// The transports a client selects between. tsnet and HTTPS look the same
// from the client's side: an address it dials over the network.
const (
	TransportUnix Transport = "unix"
	TransportHTTP Transport = "http"
)

// socketBaseURL is the URL the generated clients are given when every
// connection goes through a unix socket. Its host is a placeholder that is
// never resolved.
const socketBaseURL = "http://dagger"

// Client is the three dagger.v1 service clients over one transport.
type Client struct {
	Graph  daggerv1connect.GraphServiceClient
	Work   daggerv1connect.WorkServiceClient
	Events daggerv1connect.EventServiceClient

	transport Transport
	target    string
}

// New builds a client for cfg. An Address selects the network transport; a
// Socket, or nothing, selects the unix socket. Nothing is dialled until the
// first call.
func New(cfg Config, opts ...connect.ClientOption) (*Client, error) {
	httpClient, transport, target, err := newHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	base := socketBaseURL
	if transport == TransportHTTP {
		base = target
	}
	return &Client{
		Graph:     daggerv1connect.NewGraphServiceClient(httpClient, base, opts...),
		Work:      daggerv1connect.NewWorkServiceClient(httpClient, base, opts...),
		Events:    daggerv1connect.NewEventServiceClient(httpClient, base, opts...),
		transport: transport,
		target:    target,
	}, nil
}

// Transport reports which transport the client was built over.
func (c *Client) Transport() Transport { return c.transport }

// Target is the socket path or the address the client connects to.
func (c *Client) Target() string { return c.target }

// newHTTPClient picks the transport. No timeout is set on the client: a
// waiting ClaimNext legitimately outlives any fixed bound, and callers
// bound individual calls with their context.
func newHTTPClient(cfg Config) (*http.Client, Transport, string, error) {
	if cfg.Address != "" {
		u, err := url.Parse(cfg.Address)
		if err != nil {
			return nil, "", "", fmt.Errorf("client: address %q: %w", cfg.Address, err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, "", "", fmt.Errorf("client: address %q must be an http or https URL", cfg.Address)
		}
		return &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}, TransportHTTP, u.String(), nil
	}

	socket := cfg.Socket
	if socket == "" {
		socket = DefaultSocketPath()
	}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "unix", socket)
		if err != nil {
			return nil, fmt.Errorf("dagger is not reachable at %s (is dg serve running?): %w", socket, errors.Unwrap(err))
		}
		return conn, nil
	}
	return &http.Client{Transport: &http.Transport{DialContext: dial}}, TransportUnix, socket, nil
}
