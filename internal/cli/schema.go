package cli

import (
	"strings"
	"sync"

	"google.golang.org/protobuf/reflect/protoreflect"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/internal/codec"
	"github.com/steading-ai/skift/internal/protodoc"
)

// The JSON Schema for a patch's operations, derived from the PatchOp
// descriptor rather than written out. A model calling apply_patch is told
// the shape of every operation, its fields, and the values its enums
// accept, and none of that can drift from the contract because none of it
// is stated twice: the schema is built at startup by walking the same
// descriptor the server decodes against.

// targetFields are the fields by which an operation names what it acts on.
// An operation carrying any of them requires them; an operation carrying
// none of them is a create, which requires a title.
var targetFields = []protoreflect.Name{"node", "from", "to", "comment_id"}

// opsProperty is the schema of the "ops" argument of apply_patch: an array
// whose items are a oneOf over the forms of skift.v1.PatchOp, one branch
// per operation, each a single-key object naming that operation.
var opsProperty = sync.OnceValue(func() map[string]any {
	field := (&skiftv1.ApplyPatchRequest{}).ProtoReflect().Descriptor().Fields().ByName("ops")
	ops := field.Message().Oneofs().ByName("op").Fields()
	branches := make([]any, 0, ops.Len())
	for i := range ops.Len() {
		op := ops.Get(i)
		branches = append(branches, map[string]any{
			"type":                 "object",
			"properties":           map[string]any{string(op.Name()): messageSchema(op.Message(), 0)},
			"required":             []string{string(op.Name())},
			"additionalProperties": false,
		})
	}
	return described(map[string]any{
		"type":     "array",
		"minItems": 1,
		"items":    map[string]any{"oneOf": branches},
	}, field.FullName())
})

// maxSchemaDepth is how far into nested messages the schema is expanded.
// Below it a message is described by its type name alone: the operations
// themselves and their immediate payloads are what a caller composes by
// hand, and spelling out every leaf costs more context than it returns.
const maxSchemaDepth = 2

// messageSchema is the schema of one message: its fields, the fields it
// requires, and the one-line summary of what it is.
func messageSchema(md protoreflect.MessageDescriptor, depth int) map[string]any {
	if depth >= maxSchemaDepth {
		return described(map[string]any{"type": "object"}, md.FullName())
	}
	fields := md.Fields()
	properties := make(map[string]any, fields.Len())
	var required []string
	for i := range fields.Len() {
		fd := fields.Get(i)
		properties[string(fd.Name())] = described(fieldSchema(fd, depth), fd.FullName())
	}
	for _, name := range targetFields {
		if fields.ByName(name) != nil {
			required = append(required, string(name))
		}
	}
	if required == nil && fields.ByName("title") != nil {
		required = []string{"title"}
	}
	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	return described(schema, md.FullName())
}

// fieldSchema is the schema of one field: its JSON type, the values it
// accepts when it is an enum, and its element type when it repeats.
func fieldSchema(fd protoreflect.FieldDescriptor, depth int) map[string]any {
	switch {
	case fd.IsMap():
		return map[string]any{"type": "object"}
	case fd.IsList():
		return map[string]any{"type": "array", "items": valueSchema(fd, depth)}
	default:
		return valueSchema(fd, depth)
	}
}

// valueSchema is the schema of a single value of fd, ignoring whether the
// field repeats.
func valueSchema(fd protoreflect.FieldDescriptor, depth int) map[string]any {
	switch fd.Kind() {
	case protoreflect.EnumKind:
		return map[string]any{"type": "string", "enum": codec.EnumNames(fd.Enum())}
	case protoreflect.MessageKind, protoreflect.GroupKind:
		if strings.HasPrefix(string(fd.Message().FullName()), "google.protobuf.") {
			return map[string]any{"type": "object"}
		}
		return messageSchema(fd.Message(), depth+1)
	case protoreflect.BoolKind:
		return map[string]any{"type": "boolean"}
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return map[string]any{"type": "number"}
	case protoreflect.StringKind, protoreflect.BytesKind:
		return map[string]any{"type": "string"}
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Uint64Kind,
		protoreflect.Fixed64Kind, protoreflect.Sfixed64Kind:
		// Proto JSON writes a 64-bit integer as a string, since JSON
		// numbers cannot hold one exactly, and reads either spelling.
		return map[string]any{"type": []string{"integer", "string"}}
	default:
		return map[string]any{"type": "integer"}
	}
}

// described gives a schema the one-line description the proto writes above
// what it was derived from, so the contract documents itself once. A field
// whose type is a message is described twice, and the words written about
// the field win: they are the more specific.
func described(schema map[string]any, name protoreflect.FullName) map[string]any {
	if summary := protodoc.Summary(name); summary != "" {
		schema["description"] = summary
	}
	return schema
}
