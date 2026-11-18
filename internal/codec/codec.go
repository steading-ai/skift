// Package codec is skift's JSON encoding of the wire messages, and the
// one place the mapping between JSON and proto lives.
//
// It differs from protojson in two ways, both so that JSON says what every
// other skift surface says. Fields carry their proto names, which are the
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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectproto"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
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
	md := m.ProtoReflect().Descriptor()
	canonical, err := rewriteMessage(raw, md, toCanonical)
	switch {
	case err == nil:
	case !json.Valid(raw):
		// The document is not JSON at all. protojson says so better than
		// the walk can.
		canonical = raw
	default:
		// A spelling that names no value of its enum. protojson would
		// treat it as an absent field and hand back the default.
		return explain(raw, md, opts.DiscardUnknown, err)
	}
	if err := opts.Unmarshal(canonical, m); err != nil {
		return explain(raw, md, opts.DiscardUnknown, err)
	}
	return nil
}

// explain replaces a decoder's error with one that names the field at
// fault and what it takes, when [Diagnose] can find it. What the decoder
// said stands when nothing in the document explains itself better.
func explain(raw []byte, md protoreflect.MessageDescriptor, allowUnknown bool, err error) error {
	if found := (diagnosis{allowUnknown: allowUnknown}).message(raw, md, ""); found != nil {
		return found
	}
	return err
}

// Name is the codec name Connect negotiates JSON under. Connect drops a
// utf-8 charset from the content type before matching, so this one name
// also covers a client that spells it.
const Name = "json"

// Codec is [Marshal] and [Unmarshal] as a Connect codec, registered under
// [Name].
type Codec struct{}

var _ connect.StableCodec = Codec{}

// HandlerOptions register the codec on a Connect handler in place of the
// default protojson one. The list replaces Connect's defaults, so the
// binary protobuf codec is named beside it.
func HandlerOptions() []connecthttp.Option {
	return []connecthttp.Option{connecthttp.WithCodecs(connectproto.NewBinaryCodec(), Codec{})}
}

func (Codec) Name() string { return Name }

func (c Codec) MarshalWrite(_ context.Context, dst io.Writer, message any) error {
	m, ok := message.(proto.Message)
	if !ok {
		return fmt.Errorf("codec: %T is not a proto message", message)
	}
	raw, err := Marshal(m)
	if err != nil {
		return err
	}
	_, err = dst.Write(raw)
	return err
}

// MarshalWriteStable is MarshalWrite, which is already compact and ordered
// by field index: Connect asks for it when a message goes in a URL.
func (c Codec) MarshalWriteStable(ctx context.Context, dst io.Writer, message any) error {
	return c.MarshalWrite(ctx, dst, message)
}

// IsBinary reports that the encoding is text, so Connect may put it in a
// URL unescaped.
func (Codec) IsBinary() bool { return false }

func (c Codec) UnmarshalRead(_ context.Context, src io.Reader, message any) error {
	raw, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	return c.unmarshal(raw, message)
}

func (Codec) unmarshal(raw []byte, message any) error {
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
