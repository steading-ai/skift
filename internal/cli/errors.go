package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"connectrpc.com/connect"
)

// Every failure leaves the binary as a JSON object on stderr and a non-zero
// exit code, so an agent driving dg from a shell branches on the code
// rather than parsing prose. The codes are Connect's, spelled as Connect
// spells them (failed_precondition, not_found, ...), because most failures
// are the service's answer passed through; a failure the client detects
// itself borrows the nearest one.

// Exit codes. A usage error is distinguished so a wrapper can tell "the
// command was wrong" from "the command failed".
const (
	ExitFailure = 1
	ExitUsage   = 2
)

// Error is the shape of the object written to stderr.
type Error struct {
	// Code is a Connect code name such as "failed_precondition".
	Code string `json:"code"`
	// Message is the human-readable cause.
	Message string `json:"message"`
	// Details carries any error details the service attached, as JSON.
	Details []json.RawMessage `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// usageError marks a mistake in how the command was invoked: a bad flag,
// the wrong arguments, a malformed reference, a missing project.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

func usage(format string, args ...any) error {
	return &usageError{err: fmt.Errorf(format, args...)}
}

// ToError renders any error as the structured form.
func ToError(err error) *Error {
	var out *Error
	if errors.As(err, &out) {
		return out
	}
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		e := &Error{Code: cerr.Code().String(), Message: cerr.Message()}
		for _, d := range cerr.Details() {
			msg, err := d.Value()
			if err != nil {
				continue
			}
			if raw, err := jsonMarshaler.Marshal(msg); err == nil {
				e.Details = append(e.Details, raw)
			}
		}
		return e
	}
	var usage *usageError
	switch {
	case errors.As(err, &usage):
		return &Error{Code: connect.CodeInvalidArgument.String(), Message: err.Error()}
	case errors.Is(err, ErrNotImplemented):
		return &Error{Code: connect.CodeUnimplemented.String(), Message: err.Error()}
	}
	return &Error{Code: connect.CodeUnknown.String(), Message: err.Error()}
}

// ExitCode is the process exit status for err.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var usage *usageError
	if errors.As(err, &usage) {
		return ExitUsage
	}
	return ExitFailure
}

// writeError writes err to w as one JSON object on one line.
func writeError(w io.Writer, err error) {
	raw, marshalErr := json.Marshal(ToError(err))
	if marshalErr != nil {
		raw = fmt.Appendf(nil, `{"code":"unknown","message":%q}`, err.Error())
	}
	// stderr failing leaves nowhere to report it.
	_, _ = w.Write(append(raw, '\n'))
}
