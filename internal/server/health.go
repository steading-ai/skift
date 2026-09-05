package server

import (
	"context"
	"encoding/json"
	"net/http"
)

// Health is the body of the health check.
type Health struct {
	// Status is "ok" when the server can do work, "draining" once shutdown
	// has begun, and "degraded" when the database does not answer.
	Status string `json:"status"`
	// Database is "ok" or the error the probe returned.
	Database string `json:"database"`
}

// health answers GET /healthz. It pings the database on every call so that
// what it reports is reachability now, not at startup. The HTTP status is
// 200 only when the service can take work.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	h := Health{Status: "ok", Database: "ok"}
	code := http.StatusOK

	ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
	defer cancel()
	if s.db == nil {
		h.Database = "not configured"
		h.Status = "degraded"
		code = http.StatusServiceUnavailable
	} else if err := s.db.Ping(ctx); err != nil {
		h.Database = err.Error()
		h.Status = "degraded"
		code = http.StatusServiceUnavailable
	}

	select {
	case <-Draining(r.Context()):
		h.Status = "draining"
		code = http.StatusServiceUnavailable
	default:
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(h); err != nil {
		s.log.Warn("write health response", "err", err)
	}
}
