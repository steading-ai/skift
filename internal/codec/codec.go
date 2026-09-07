// Package codec is dagger's JSON encoding of the wire messages, and the
// one place the mapping between JSON and proto lives.
//
// It differs from protojson in two ways, both so that JSON says what every
// other dagger surface says. Fields carry their proto names, which are the
// names docs/domain-model.md uses. Enums are the short lowercase form —
// "p2", "open", "user" — which is the CLI's flag, the display's word, and
// the model's vocabulary; the canonical name and the unprefixed name are
// accepted too, so nothing that used to be sent stops working.
//
// The rule is derived from the enum descriptor rather than listed per
// enum, so an enum added to the schema is covered by having been added.
// See docs/adrs/008-short-enum-names-on-the-json-wire.md.
package codec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// indent is what MarshalIndent indents nested objects by.
const indent = "  "

// Marshal renders m as one line of JSON.
func Marshal(m proto.Message) ([]byte, error) {
	raw, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
	if err != nil {
		return nil, err
	}
	// protojson's whitespace is deliberately unstable; compact it so that
	// what the walk copies through is a line, and a line is a line.
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, err
	}
	return rewriteMessage(compact.Bytes(), m.ProtoReflect().Descriptor(), toShort)
}

// MarshalIndent is Marshal laid out over several lines, for a person.
func MarshalIndent(m proto.Message) ([]byte, error) {
	raw, err := Marshal(m)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", indent); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Unmarshal parses JSON into m, accepting every spelling of an enum value
// that names it: "PRIORITY_P2", "P2", or "p2".
func Unmarshal(raw []byte, m proto.Message) error {
	return unmarshal(raw, m, protojson.UnmarshalOptions{})
}

func unmarshal(raw []byte, m proto.Message, opts protojson.UnmarshalOptions) error {
	canonical, err := rewriteMessage(raw, m.ProtoReflect().Descriptor(), toCanonical)
	switch {
	case err == nil:
	case !json.Valid(raw):
		// The document is not JSON at all. protojson says so better than
		// the walk can.
		canonical = raw
	default:
		// A spelling that names no value of its enum. protojson would
		// treat it as an absent field and hand back the default.
		return err
	}
	return opts.Unmarshal(canonical, m)
}

// Name is the codec name Connect negotiates JSON under.
const Name = "json"

// nameUTF8 is the same codec under the content type a client may spell
// with a charset. Connect treats the two as separate registrations.
const nameUTF8 = Name + "; charset=utf-8"

// Codec is [Marshal] and [Unmarshal] as a Connect codec, registered under
// one of the JSON names.
type Codec struct{ name string }

var (
	_ connect.Codec = Codec{}
	_ interface {
		MarshalStable(any) ([]byte, error)
		IsBinary() bool
	} = Codec{}
)

// New is the codec registered under name.
func New(name string) Codec { return Codec{name: name} }

// HandlerOptions register the codec on a Connect handler in place of the
// default protojson one, under both names Connect knows JSON by.
func HandlerOptions() []connect.HandlerOption {
	return []connect.HandlerOption{connect.WithCodec(New(Name)), connect.WithCodec(New(nameUTF8))}
}

func (c Codec) Name() string { return c.name }

func (c Codec) Marshal(message any) ([]byte, error) {
	m, ok := message.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("codec: %T is not a proto message", message)
	}
	return Marshal(m)
}

// MarshalStable is Marshal, which is already compact and ordered by field
// index: Connect asks for it when a message goes in a URL.
func (c Codec) MarshalStable(message any) ([]byte, error) { return c.Marshal(message) }

// IsBinary reports that the encoding is text, so Connect may put it in a
// URL unescaped.
func (c Codec) IsBinary() bool { return false }

func (c Codec) Unmarshal(raw []byte, message any) error {
	m, ok := message.(proto.Message)
	if !ok {
		return fmt.Errorf("codec: %T is not a proto message", message)
	}
	if len(raw) == 0 {
		return errors.New("zero-length payload is not a valid JSON object")
	}
	// Unknown fields are discarded on the wire, as Connect's own JSON
	// codec does, so a client and a server need not share a schema
	// version exactly.
	if err := unmarshal(raw, m, protojson.UnmarshalOptions{DiscardUnknown: true}); err != nil {
		return fmt.Errorf("unmarshal into %T: %w", message, err)
	}
	return nil
}
