package codec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// Why a decode failed, said in terms of the message it failed against.
//
// A raw protojson error names the problem and withholds the answer:
// `unknown field "ref"` does not say that the field is called "node", and
// `invalid value for enum field priority` does not say that p0 through p4
// are what it takes. Both are in the descriptor at the point of failure.
// An agent reads a tool error as its next prompt, so an error that carries
// the fix costs one retry and one that does not costs several.
//
// The walk here mirrors the rewrite in json.go — the same document beside
// the same descriptors — but it runs only once a decode has already
// failed, and its whole output is the first thing it finds wrong, with the
// JSON path to it: ops[3].update_node.

// diagnosis is one pass over a document, yielding the first thing it finds
// wrong or nothing at all. Unknown fields are only worth reporting when
// the decoder would have refused them; on the wire they are discarded on
// purpose, so a walk run for another reason must not raise them.
type diagnosis struct{ allowUnknown bool }

// message reads raw as md, at path. A document that is not JSON at all, or
// not JSON the walk can follow, yields no diagnosis: the decoder says that
// better than this can.
func (d diagnosis) message(raw json.RawMessage, md protoreflect.MessageDescriptor, path string) error {
	if md == nil || strings.HasPrefix(string(md.FullName()), wellKnown) || isNull(raw) {
		return nil
	}
	if opening(raw) != '{' {
		return typeError(path, "an object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return nil
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil
		}
		key, ok := tok.(string)
		if !ok {
			return nil
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil
		}
		fd := field(md, key)
		if fd == nil {
			if d.allowUnknown {
				continue
			}
			return fmt.Errorf("%sunknown field %q; fields are %s", prefixOf(path), key, strings.Join(fieldNames(md), ", "))
		}
		if err := d.field(value, fd, join(path, string(fd.Name()))); err != nil {
			return err
		}
	}
	return nil
}

// field reads the value of one field, unwrapping the array or the object a
// repeated field carries. An element of a list keeps the list's path, so
// the third operation of a patch is ops[2] and not ops.ops[2].
func (d diagnosis) field(raw json.RawMessage, fd protoreflect.FieldDescriptor, path string) error {
	switch {
	case isNull(raw):
		return nil
	case fd.IsList():
		if opening(raw) != '[' {
			return typeError(path, "an array of "+plural(fd))
		}
		return d.each(raw, fd, path)
	case fd.IsMap():
		if opening(raw) != '{' {
			return typeError(path, "an object")
		}
		return d.entries(raw, fd.MapValue(), path)
	default:
		return d.value(raw, fd, path)
	}
}

// each reads every element of a repeated field's array.
func (d diagnosis) each(raw json.RawMessage, fd protoreflect.FieldDescriptor, path string) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return nil
	}
	for i := 0; dec.More(); i++ {
		var element json.RawMessage
		if err := dec.Decode(&element); err != nil {
			return nil
		}
		if err := d.value(element, fd, fmt.Sprintf("%s[%d]", path, i)); err != nil {
			return err
		}
	}
	return nil
}

// entries reads the values of a map field. Map keys are never enums or
// messages, so only the values are walked.
func (d diagnosis) entries(raw json.RawMessage, value protoreflect.FieldDescriptor, path string) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return nil
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil
		}
		key, ok := tok.(string)
		if !ok {
			return nil
		}
		var element json.RawMessage
		if err := dec.Decode(&element); err != nil {
			return nil
		}
		if err := d.value(element, value, join(path, key)); err != nil {
			return err
		}
	}
	return nil
}

// value reads one value of fd's own type.
func (d diagnosis) value(raw json.RawMessage, fd protoreflect.FieldDescriptor, path string) error {
	if isNull(raw) {
		return nil
	}
	switch fd.Kind() {
	case protoreflect.EnumKind:
		return enumError(raw, fd.Enum(), path)
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return d.message(raw, fd.Message(), path)
	case protoreflect.BoolKind:
		if o := opening(raw); o != 't' && o != 'f' {
			return typeError(path, "true or false")
		}
	case protoreflect.StringKind, protoreflect.BytesKind:
		if opening(raw) != '"' {
			return typeError(path, "a string")
		}
	default:
		// Numbers: protojson takes the quoted form for the 64-bit kinds
		// and for infinities, so a string is not by itself wrong.
		if o := opening(raw); o != '"' && o != '-' && (o < '0' || o > '9') {
			return typeError(path, "a number")
		}
	}
	return nil
}

// enumError reports a string that names no value of ed, and takes a number
// as protojson does.
func enumError(raw json.RawMessage, ed protoreflect.EnumDescriptor, path string) error {
	if opening(raw) != '"' {
		if o := opening(raw); o == '-' || (o >= '0' && o <= '9') {
			return nil
		}
		return typeError(path, "one of "+strings.Join(EnumNames(ed), ", "))
	}
	var spelling string
	if err := json.Unmarshal(raw, &spelling); err != nil {
		return nil
	}
	if _, ok := Value(ed, spelling); ok {
		return nil
	}
	return fmt.Errorf("%s%q is not one of %s", prefixOf(path), spelling, strings.Join(EnumNames(ed), ", "))
}

// typeError says what the field at path takes.
func typeError(path, expected string) error {
	return fmt.Errorf("%sexpected %s", prefixOf(path), expected)
}

// fieldNames lists a message's fields by their proto names, in the order
// the schema declares them: what the caller may write here.
func fieldNames(md protoreflect.MessageDescriptor) []string {
	names := make([]string, 0, md.Fields().Len())
	for i := range md.Fields().Len() {
		names = append(names, string(md.Fields().Get(i).Name()))
	}
	return names
}

// plural describes the elements of a repeated field, for the message that
// says an array was expected.
func plural(fd protoreflect.FieldDescriptor) string {
	switch fd.Kind() {
	case protoreflect.EnumKind:
		return "values, each one of " + strings.Join(EnumNames(fd.Enum()), ", ")
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return "objects"
	case protoreflect.BoolKind:
		return "booleans"
	case protoreflect.StringKind, protoreflect.BytesKind:
		return "strings"
	default:
		return "numbers"
	}
}

// join extends a JSON path by one field name.
func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// prefixOf is the path as an error prefix, absent at the root.
func prefixOf(path string) string {
	if path == "" {
		return ""
	}
	return path + ": "
}

// isNull reports whether raw is JSON null, which every field accepts.
func isNull(raw json.RawMessage) bool {
	return opening(raw) == 'n'
}
