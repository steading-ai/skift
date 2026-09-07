// Package protodoc reads the comments in dagger's own proto files at
// runtime.
//
// The comments on a message and its fields are the contract's
// documentation, and a tool schema generated from a descriptor wants them:
// they are what tells a model what an operation is for. protoc-gen-go
// strips source information from the descriptors it embeds, so
// protoreflect cannot reach them. The build writes a descriptor set that
// keeps it, beside this file, and this package reads that.
package protodoc

import (
	_ "embed"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// descriptors is every proto file of this module and its dependencies,
// with source information. Written by `make generate`; `make lint` fails
// when it lags proto/.
//
//go:embed descriptors.binpb
var descriptors []byte

// Field numbers of the descriptor messages, which are what a source
// location's path is expressed in. See the comment on
// SourceCodeInfo.Location in descriptor.proto.
const (
	fileMessages  = 4
	fileEnums     = 5
	messageField  = 2
	messageNested = 3
	messageEnums  = 4
	enumValue     = 2
)

// comments maps the full name of a message, field, enum, or enum value to
// the comment written above it.
var comments = sync.OnceValue(func() map[protoreflect.FullName]string {
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(descriptors, &set); err != nil {
		// The descriptor set is embedded from the build, so a failure
		// here is a broken binary rather than a runtime condition.
		panic("protodoc: embedded descriptor set is unreadable: " + err.Error())
	}
	out := map[protoreflect.FullName]string{}
	for _, file := range set.GetFile() {
		names := map[string]protoreflect.FullName{}
		pkg := protoreflect.FullName(file.GetPackage())
		for i, message := range file.GetMessageType() {
			indexMessage(names, path(fileMessages, i), pkg.Append(protoreflect.Name(message.GetName())), message)
		}
		for i, enum := range file.GetEnumType() {
			indexEnum(names, path(fileEnums, i), pkg.Append(protoreflect.Name(enum.GetName())), enum)
		}
		for _, location := range file.GetSourceCodeInfo().GetLocation() {
			name, ok := names[key(location.GetPath())]
			if !ok || location.GetLeadingComments() == "" {
				continue
			}
			out[name] = location.GetLeadingComments()
		}
	}
	return out
})

// indexMessage records where a message, its fields, and everything nested
// in it are found, so a source location can be resolved to a name.
func indexMessage(names map[string]protoreflect.FullName, at []int, name protoreflect.FullName, message *descriptorpb.DescriptorProto) {
	names[key(at)] = name
	for i, field := range message.GetField() {
		names[key(sub(at, messageField, i))] = name.Append(protoreflect.Name(field.GetName()))
	}
	for i, nested := range message.GetNestedType() {
		indexMessage(names, sub(at, messageNested, i), name.Append(protoreflect.Name(nested.GetName())), nested)
	}
	for i, enum := range message.GetEnumType() {
		indexEnum(names, sub(at, messageEnums, i), name.Append(protoreflect.Name(enum.GetName())), enum)
	}
}

// indexEnum records where an enum and its values are found. A value is
// scoped to whatever encloses its enum, not to the enum, which is how
// protoreflect names it.
func indexEnum(names map[string]protoreflect.FullName, at []int, name protoreflect.FullName, enum *descriptorpb.EnumDescriptorProto) {
	names[key(at)] = name
	for i, value := range enum.GetValue() {
		names[key(sub(at, enumValue, i))] = name.Parent().Append(protoreflect.Name(value.GetName()))
	}
}

// path is a source location path.
func path(elements ...int) []int {
	return elements
}

// sub is the path of something declared inside what at names, copied so
// that no two siblings share a backing array.
func sub(at []int, elements ...int) []int {
	out := make([]int, 0, len(at)+len(elements))
	return append(append(out, at...), elements...)
}

// key renders a source location path as a map key.
func key[E int | int32](elements []E) string {
	parts := make([]string, len(elements))
	for i, e := range elements {
		parts[i] = strconv.Itoa(int(e))
	}
	return strings.Join(parts, ".")
}

// leadLength is how long a summary has to be before it is allowed to stop.
// A field documented as "Markdown." says nothing on its own; the sentence
// after it is the one that carries the point.
const leadLength = 40

// Summary is the opening of the comment on the named message, field, enum,
// or enum value, as one line: its first sentence, and the sentences after
// it until there is something to read. It is empty when nothing is
// documented there.
func Summary(name protoreflect.FullName) string {
	comment := strings.Join(strings.Fields(comments()[name]), " ")
	for at := 0; at < len(comment); {
		stop := strings.Index(comment[at:], ". ")
		if stop < 0 {
			break
		}
		at += stop + 1
		if at >= leadLength {
			return comment[:at]
		}
		at++
	}
	return comment
}
