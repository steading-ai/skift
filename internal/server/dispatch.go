package server

import (
	"net/http"
	"net/http/httptest"

	"connectrpc.com/connect"
)

// InProcessClient returns a unary Connect dispatcher for generated Graph and
// Work clients. It invokes the very same handlers as network Connect, including
// server-side interceptors and the response codec, without opening a socket.
// Each generated request must use the original tool callback context: that
// context carries the accepted peer, cancellation, deadline, and drain signal.
// Events, health, and MCP are deliberately absent from this mux.
func (s *Server) InProcessClient() connect.HTTPClient {
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
