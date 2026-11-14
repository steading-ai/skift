package mcptools

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"connectrpc.com/connect"

	"github.com/steading-ai/skift/internal/codec"
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
			if raw, err := codec.Marshal(msg); err == nil {
				e.Details = append(e.Details, raw)
			}
		}
		return e
	}
	var usage *usageError
	if errors.As(err, &usage) {
		return &Error{Code: connect.CodeInvalidArgument.String(), Message: err.Error()}
	}
	return &Error{Code: connect.CodeUnknown.String(), Message: err.Error()}
}

// Decode failures are the caller's next prompt. A decoder says what it
// could not do; these say what to write instead, naming the JSON path, the
// field at fault, and the values or the fields the schema allows. The
// codec does that for a proto message; the same is done here for the Go
// structs the MCP tools decode their arguments into, so both doors answer
// alike. AGENT_REVIEW.md §9.

// decodeUsage reports a decode failure as a usage error. The codec's
// message already carries the path and the legal values, so nothing is
// prefixed to it: skift's stderr object and the MCP tool error say the same
// sentence about the same patch.
func decodeUsage(err error) error { return &usageError{err: err} }

// unknownField matches the error encoding/json gives for a field the
// target struct does not have. It carries the name and nothing else.
var unknownField = regexp.MustCompile(`^json: unknown field "(.*)"$`)

// argsError renders a failure to decode tool arguments into the Go value
// target, naming the legal fields for an unknown one and the expected type
// for a mistyped one.
func argsError(err error, target any) error {
	var typeErr *json.UnmarshalTypeError
	switch {
	case unknownField.MatchString(err.Error()):
		name := unknownField.FindStringSubmatch(err.Error())[1]
		return usage("unknown field %q; fields are %s", name, strings.Join(argFields(target), ", "))
	case errors.As(err, &typeErr):
		return usage("%sexpected %s", prefixOf(typeErr.Field), goType(typeErr.Type))
	}
	return usage("arguments: %v", err)
}

// argFields lists the JSON names of a struct's fields, in declaration
// order, following embedded structs as encoding/json does.
func argFields(target any) []string {
	t := reflect.TypeOf(target)
	for t != nil && (t.Kind() == reflect.Pointer || t.Kind() == reflect.Interface) {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	var names []string
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch {
		case name == "-":
		case f.Anonymous && name == "":
			names = append(names, argFields(reflect.New(f.Type).Interface())...)
		case name != "":
			names = append(names, name)
		case f.IsExported():
			names = append(names, f.Name)
		}
	}
	return names
}

// goType describes a Go type as the JSON it is decoded from.
func goType(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "true or false"
	case reflect.Slice, reflect.Array:
		return "an array of " + plural(t.Elem())
	case reflect.Map, reflect.Struct:
		return "an object"
	default:
		return "a number"
	}
}

// plural describes the elements of an array.
func plural(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "strings"
	case reflect.Bool:
		return "booleans"
	case reflect.Map, reflect.Struct:
		return "objects"
	case reflect.Slice, reflect.Array:
		return "arrays"
	default:
		return "numbers"
	}
}

// prefixOf is a JSON path as an error prefix, absent at the root.
func prefixOf(path string) string {
	if path == "" {
		return ""
	}
	return path + ": "
}
