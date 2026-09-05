// Package logging builds the service's structured logger.
//
// The service logs through log/slog. Under systemd its stderr is the
// journal, and each line is prefixed with an sd-daemon priority so journald
// records the level as a field rather than as text. Anywhere else the same
// records go out as plain key=value text.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync"
)

// journalStreamEnv is set by systemd on a service whose stdout or stderr is
// connected to the journal.
const journalStreamEnv = "JOURNAL_STREAM"

// New returns a logger writing to w at level. When the process is running
// under the journal, records carry syslog priority prefixes.
func New(w io.Writer, level slog.Leveler) *slog.Logger {
	return slog.New(NewHandler(w, level, UnderJournal()))
}

// UnderJournal reports whether systemd connected this process to journald.
func UnderJournal() bool {
	return os.Getenv(journalStreamEnv) != ""
}

// NewHandler builds the handler behind [New]. The journal flag is explicit so
// specs can exercise both shapes without touching the environment.
func NewHandler(w io.Writer, level slog.Leveler, journal bool) slog.Handler {
	opts := &slog.HandlerOptions{Level: level}
	if !journal {
		return slog.NewTextHandler(w, opts)
	}
	pw := &prefixWriter{w: w}
	return &journalHandler{
		mu:    &sync.Mutex{},
		pw:    pw,
		inner: slog.NewTextHandler(pw, opts),
	}
}

// journalHandler prefixes each record with its journald priority. The text
// handler writes one line per record, so the prefix set just before Handle
// lands at the start of that line. The mutex makes the prefix and the write
// one step, and is shared by every handler derived through WithAttrs and
// WithGroup, which write through the same prefixWriter.
type journalHandler struct {
	mu    *sync.Mutex
	pw    *prefixWriter
	inner slog.Handler
}

func (h *journalHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *journalHandler) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pw.prefix = Priority(r.Level)
	return h.inner.Handle(ctx, r)
}

func (h *journalHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &journalHandler{mu: h.mu, pw: h.pw, inner: h.inner.WithAttrs(attrs)}
}

func (h *journalHandler) WithGroup(name string) slog.Handler {
	return &journalHandler{mu: h.mu, pw: h.pw, inner: h.inner.WithGroup(name)}
}

// Priority maps a slog level to the sd-daemon prefix journald parses:
// "<7>" debug, "<6>" info, "<4>" warning, "<3>" error.
func Priority(level slog.Level) string {
	var p int
	switch {
	case level >= slog.LevelError:
		p = 3
	case level >= slog.LevelWarn:
		p = 4
	case level >= slog.LevelInfo:
		p = 6
	default:
		p = 7
	}
	return "<" + strconv.Itoa(p) + ">"
}

// prefixWriter writes prefix ahead of every write. Callers serialise through
// journalHandler.mu.
type prefixWriter struct {
	w      io.Writer
	prefix string
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	line := make([]byte, 0, len(p.prefix)+len(b))
	line = append(line, p.prefix...)
	line = append(line, b...)
	if _, err := p.w.Write(line); err != nil {
		return 0, err
	}
	return len(b), nil
}
