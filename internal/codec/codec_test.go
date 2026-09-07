package codec_test

import (
	"fmt"
	"strings"
	"unicode"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/internal/codec"
)

// The codec is checked over the schema rather than over a list: every enum
// of every dagger.v1 file, every value of each, through a message field
// that carries it. Nothing here names an enum, so an enum added to the
// schema is covered the day it is added.

// files are the schema's proto files.
func files() []protoreflect.FileDescriptor {
	return []protoreflect.FileDescriptor{
		daggerv1.File_dagger_v1_dagger_proto,
		daggerv1.File_dagger_v1_service_proto,
	}
}

// enums is every enum declared in the schema, top level or nested.
func enums() []protoreflect.EnumDescriptor {
	var found []protoreflect.EnumDescriptor
	var fromMessages func(protoreflect.MessageDescriptors)
	fromMessages = func(ms protoreflect.MessageDescriptors) {
		for i := range ms.Len() {
			m := ms.Get(i)
			for j := range m.Enums().Len() {
				found = append(found, m.Enums().Get(j))
			}
			fromMessages(m.Messages())
		}
	}
	for _, f := range files() {
		for i := range f.Enums().Len() {
			found = append(found, f.Enums().Get(i))
		}
		fromMessages(f.Messages())
	}
	return found
}

// carrier is a singular message field of the given enum: where a value of
// it is put to see it encoded and read back.
func carrier(ed protoreflect.EnumDescriptor) protoreflect.FieldDescriptor {
	var found protoreflect.FieldDescriptor
	var fromMessages func(protoreflect.MessageDescriptors)
	fromMessages = func(ms protoreflect.MessageDescriptors) {
		for i := range ms.Len() {
			m := ms.Get(i)
			for j := range m.Fields().Len() {
				f := m.Fields().Get(j)
				if found == nil && f.Kind() == protoreflect.EnumKind && !f.IsList() && !f.IsMap() && f.Enum().FullName() == ed.FullName() {
					found = f
				}
			}
			fromMessages(m.Messages())
		}
	}
	for _, f := range files() {
		fromMessages(f.Messages())
	}
	return found
}

// screamingPrefix is the prefix an enum's values carry by proto
// convention, derived here independently of the codec: the enum's name
// split at its capitals, upper-cased, with a trailing underscore.
func screamingPrefix(ed protoreflect.EnumDescriptor) string {
	var words []string
	word := ""
	for _, r := range string(ed.Name()) {
		if unicode.IsUpper(r) && word != "" && !isAllUpper(word) {
			words = append(words, word)
			word = ""
		}
		word += string(r)
	}
	return strings.ToUpper(strings.Join(append(words, word), "_")) + "_"
}

func isAllUpper(s string) bool {
	for _, r := range s {
		if unicode.IsLower(r) {
			return false
		}
	}
	return true
}

var _ = Describe("The JSON enum codec", Label("unit"), func() {
	It("takes all three spellings of every value of every enum, and emits the short one", func() {
		Expect(enums()).NotTo(BeEmpty())
		for _, ed := range enums() {
			field := carrier(ed)
			Expect(field).NotTo(BeNil(), "%s: no message field carries this enum", ed.FullName())
			for i := range ed.Values().Len() {
				value := ed.Values().Get(i)
				canonical := string(value.Name())
				unprefixed := strings.TrimPrefix(canonical, screamingPrefix(ed))
				Expect(unprefixed).NotTo(Equal(canonical), "%s: values do not carry the enum's prefix", ed.FullName())
				short := strings.ToLower(unprefixed)

				for _, spelling := range []string{canonical, unprefixed, short} {
					msg := dynamicpb.NewMessage(field.ContainingMessage())
					raw := fmt.Sprintf(`{%q: %q}`, field.Name(), spelling)
					Expect(codec.Unmarshal([]byte(raw), msg)).To(Succeed(), "%s: %s", ed.FullName(), raw)
					Expect(msg.Get(field).Enum()).To(Equal(value.Number()), "%s: %s decoded to the wrong value", ed.FullName(), spelling)
				}

				if value.Number() == 0 {
					// The zero value is what an absent field means, so
					// it is not on the wire to be spelt.
					continue
				}
				msg := dynamicpb.NewMessage(field.ContainingMessage())
				msg.Set(field, protoreflect.ValueOfEnum(value.Number()))
				raw, err := codec.Marshal(msg)
				Expect(err).NotTo(HaveOccurred())
				Expect(string(raw)).To(Equal(fmt.Sprintf(`{%q:%q}`, field.Name(), short)), "%s: %s is not emitted short", ed.FullName(), canonical)
			}
		}
	})

	It("lists the short forms as the legal values, and leaves a spelling that names nothing to be reported", func() {
		for _, ed := range enums() {
			names := codec.EnumNames(ed)
			Expect(names).To(HaveLen(ed.Values().Len()-1), "%s: every value but the zero one", ed.FullName())
			for _, name := range names {
				Expect(name).To(Equal(strings.ToLower(name)), "%s: %s is not short", ed.FullName(), name)
				v, ok := codec.Value(ed, name)
				Expect(ok).To(BeTrue(), "%s: %s is not accepted", ed.FullName(), name)
				Expect(codec.EnumName(dynamicEnum{ed: ed, n: v.Number()})).To(Equal(name))
			}

			field := carrier(ed)
			msg := dynamicpb.NewMessage(field.ContainingMessage())
			err := codec.Unmarshal([]byte(fmt.Sprintf(`{%q: "no such value"}`, field.Name())), msg)
			Expect(err).To(HaveOccurred(), "%s: an unknown spelling is not a value", ed.FullName())
		}
	})

	It("leaves everything that is not an enum as protojson wrote it", func() {
		node := &daggerv1.Node{
			Number: 7, Title: "Ship it", Labels: []string{"a", "b"},
			Priority: daggerv1.Priority_PRIORITY_P0, Status: daggerv1.Status_STATUS_OPEN,
		}
		raw, err := codec.Marshal(node)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(Equal(`{"number":"7","title":"Ship it","status":"open","priority":"p0","labels":["a","b"]}`))

		round := &daggerv1.Node{}
		Expect(codec.Unmarshal(raw, round)).To(Succeed())
		Expect(round.GetPriority()).To(Equal(daggerv1.Priority_PRIORITY_P0))
		Expect(round.GetLabels()).To(Equal([]string{"a", "b"}))
	})
})

var _ = Describe("A decode error", Label("unit"), func() {
	// What the caller needs is the path to the value at fault and the
	// values or fields the schema allows there. Both come from the
	// descriptor, so the spec reads them from the descriptor too.
	decode := func(raw string) error {
		return codec.Unmarshal([]byte(raw), &daggerv1.ApplyPatchRequest{})
	}

	It("names the operation, the field, and every legal value of the enum", func() {
		err := decode(`{"ops": [{"create_node": {"title": "A"}}, {"create_node": {"priority": "high"}}]}`)
		legal := strings.Join(codec.EnumNames(daggerv1.Priority(0).Descriptor()), ", ")
		Expect(err).To(MatchError(`ops[1].create_node.priority: "high" is not one of ` + legal))
	})

	It("names an unknown field and lists the fields of the message it was written on", func() {
		err := decode(`{"ops": [{"update_node": {"ref": "#1"}}]}`)
		Expect(err).To(HaveOccurred())
		fields := (&daggerv1.UpdateNodeOp{}).ProtoReflect().Descriptor().Fields()
		names := make([]string, 0, fields.Len())
		for i := range fields.Len() {
			names = append(names, string(fields.Get(i).Name()))
		}
		Expect(err).To(MatchError(`ops[0].update_node: unknown field "ref"; fields are ` + strings.Join(names, ", ")))
	})

	It("says what a field of the wrong type takes", func() {
		Expect(decode(`{"ops": [{"create_node": {"blocked_by": "#1"}}]}`)).
			To(MatchError("ops[0].create_node.blocked_by: expected an array of strings"))
		Expect(decode(`{"ops": [{"create_node": {"title": 7}}]}`)).
			To(MatchError("ops[0].create_node.title: expected a string"))
		Expect(decode(`{"ops": {"create_node": {}}}`)).
			To(MatchError("ops: expected an array of objects"))
	})

	It("leaves a document that is not JSON to the decoder", func() {
		Expect(decode(`{"ops": [`)).To(HaveOccurred())
		Expect(decode(`nonsense`)).To(HaveOccurred())
	})
})

// dynamicEnum is one value of an enum known only by its descriptor, so
// that EnumName can be asked about an enum the spec does not name.
type dynamicEnum struct {
	ed protoreflect.EnumDescriptor
	n  protoreflect.EnumNumber
}

func (e dynamicEnum) Descriptor() protoreflect.EnumDescriptor { return e.ed }
func (e dynamicEnum) Type() protoreflect.EnumType             { return dynamicpb.NewEnumType(e.ed) }
func (e dynamicEnum) Number() protoreflect.EnumNumber         { return e.n }
