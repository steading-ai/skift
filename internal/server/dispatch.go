package server

import (
	"net/http"
	"net/http/httptest"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
)

// inProcessBaseURL is the base URL in-process requests are addressed to.
// Nothing resolves it; the dispatcher hands every request to the mux.
const inProcessBaseURL = "http://skift.internal"

// unboundedReads is the per-message read limit on every Connect handler
// and client: none. The domain's own ceilings bound what a request may
// carry, and a patch or a page within them can exceed Connect's default.
const unboundedReads = 0

// InProcessClient returns a unary Connect client for generated Graph and
// Work clients. It invokes the very same handlers as network Connect,
// including server-side interceptors and the response codec, without
// opening a socket. Each generated request must use the original tool
// callback context: that context carries the accepted peer, cancellation,
// deadline, and drain signal. Events, health, and MCP are deliberately
// absent from this mux.
func (s *Server) InProcessClient() *connect.Client {
	return connect.NewClient(connecthttp.NewTransport(s.InProcessHTTPClient(), inProcessBaseURL,
		connecthttp.WithReadMaxBytes(unboundedReads)))
}

// InProcessHTTPClient is the dispatcher [Server.InProcessClient] sends
// through, for a caller that builds its own requests.
func (s *Server) InProcessHTTPClient() connecthttp.HTTPClient {
	return inProcessClient{handler: s.connectMux}
}

type inProcessClient struct{ handler http.Handler }

func (c inProcessClient) Do(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	// Preserve the original context. RequestURI is normally populated by the
	// HTTP server, whereas generated clients construct outbound requests.
	r := req.Clone(req.Context())
	r.RequestURI = req.URL.RequestURI()
	if req.Body != nil {
		defer func() { _ = req.Body.Close() }()
	}
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, r)
	return w.Result(), nil
}
