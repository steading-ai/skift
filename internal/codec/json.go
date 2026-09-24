package codec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// The rewrite itself: a walk of a JSON document beside the descriptor of
// the message it encodes, changing the spelling of the strings that land
// on an enum field and copying everything else through untouched. The walk
// is driven entirely by protoreflect, so an enum added later is covered
// without anything being written for it.

// wellKnown is the package whose messages have JSON forms of their own. A
// timestamp, a duration, or a Struct is not an object of proto fields, so
// the walk stops at one rather than reading its JSON as a message.
const wellKnown = "google.protobuf."

// rewrite is how a spelling of an enum value is changed: canonical for
// decoding, short for encoding.
type rewrite func(protoreflect.EnumDescriptor, string) (string, error)

// toShort spells a value the way skift emits it.
func toShort(ed protoreflect.EnumDescriptor, name string) (string, error) {
	return Short(ed, name), nil
}

// toCanonical spells a value the way protojson requires. A spelling that
// names no value is refused here rather than left to protojson, which
// treats an unknown enum name as an absent field when it is discarding
// unknown fields: a misspelt priority would silently become the default.
func toCanonical(ed protoreflect.EnumDescriptor, spelling string) (string, error) {
	v, ok := Value(ed, spelling)
	if !ok {
		return "", fmt.Errorf("%q is not one of %s", spelling, strings.Join(EnumNames(ed), ", "))
	}
	return string(v.Name()), nil
}

// rewriteMessage rewrites raw, a JSON value read as md. Anything that is
// not an object, and every well-known type, is returned as it came.
func rewriteMessage(raw json.RawMessage, md protoreflect.MessageDescriptor, rw rewrite) (json.RawMessage, error) {
	if md == nil || strings.HasPrefix(string(md.FullName()), wellKnown) || opening(raw) != '{' {
		return raw, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteByte('{')
	for i := 0; dec.More(); i++ {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("codec: %s: object key is not a string", md.FullName())
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		if value, err = rewriteField(value, field(md, key), rw); err != nil {
			return nil, err
		}
		if i > 0 {
			out.WriteByte(',')
		}
		name, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		out.Write(name)
		out.WriteByte(':')
		out.Write(value)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

// field is the field of md a JSON key names, by either spelling protojson
// accepts. An unknown key has no field and is copied through.
func field(md protoreflect.MessageDescriptor, key string) protoreflect.FieldDescriptor {
	if fd := md.Fields().ByJSONName(key); fd != nil {
		return fd
	}
	return md.Fields().ByName(protoreflect.Name(key))
}

// rewriteField rewrites the JSON value of one field, unwrapping the list
// or the map a repeated field carries.
func rewriteField(raw json.RawMessage, fd protoreflect.FieldDescriptor, rw rewrite) (json.RawMessage, error) {
	switch {
	case fd == nil:
		return raw, nil
	case fd.IsList():
		return rewriteList(raw, fd, rw)
	case fd.IsMap():
		return rewriteMap(raw, fd.MapValue(), rw)
	default:
		return rewriteSingular(raw, fd, rw)
	}
}

// rewriteSingular rewrites one value of fd's type: an enum's spelling, a
// message's fields, or nothing at all.
func rewriteSingular(raw json.RawMessage, fd protoreflect.FieldDescriptor, rw rewrite) (json.RawMessage, error) {
	switch fd.Kind() {
	case protoreflect.EnumKind:
		value, err := rewriteEnum(raw, fd.Enum(), rw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", fd.Name(), err)
		}
		return value, nil
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return rewriteMessage(raw, fd.Message(), rw)
	default:
		return raw, nil
	}
}

// rewriteEnum respells a JSON string naming a value of ed. An enum given
// as a number, or as null, is left alone.
func rewriteEnum(raw json.RawMessage, ed protoreflect.EnumDescriptor, rw rewrite) (json.RawMessage, error) {
	if opening(raw) != '"' {
		return raw, nil
	}
	var spelling string
	if err := json.Unmarshal(raw, &spelling); err != nil {
		return nil, err
	}
	respelt, err := rw(ed, spelling)
	if err != nil {
		return nil, err
	}
	if respelt == spelling {
		return raw, nil
	}
	return json.Marshal(respelt)
}

// rewriteList rewrites every element of a repeated field's array.
func rewriteList(raw json.RawMessage, fd protoreflect.FieldDescriptor, rw rewrite) (json.RawMessage, error) {
	if opening(raw) != '[' {
		return raw, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteByte('[')
	for i := 0; dec.More(); i++ {
		var element json.RawMessage
		if err := dec.Decode(&element); err != nil {
			return nil, err
		}
		element, err := rewriteSingular(element, fd, rw)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			out.WriteByte(',')
		}
		out.Write(element)
	}
	out.WriteByte(']')
	return out.Bytes(), nil
}

// rewriteMap rewrites the values of a map field. Proto map keys are never
// enums, so only the values are walked.
func rewriteMap(raw json.RawMessage, value protoreflect.FieldDescriptor, rw rewrite) (json.RawMessage, error) {
	if opening(raw) != '{' {
		return raw, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteByte('{')
	for i := 0; dec.More(); i++ {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("codec: map key is not a string")
		}
		var element json.RawMessage
		if err := dec.Decode(&element); err != nil {
			return nil, err
		}
		if element, err = rewriteSingular(element, value, rw); err != nil {
			return nil, err
		}
		if i > 0 {
			out.WriteByte(',')
		}
		name, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		out.Write(name)
		out.WriteByte(':')
		out.Write(element)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

// opening is the first byte of a JSON value that says what it is, or zero
// when there is nothing but whitespace.
func opening(raw json.RawMessage) byte {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return b
		}
	}
	return 0
}
