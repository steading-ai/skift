package codec

import (
	"slices"
	"strconv"
	"strings"
	"unicode"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// Enum spellings, derived from the descriptor and nothing else. An enum's
// values carry the enum's own name as a prefix by proto convention, which
// is what generated-code scoping needs and what makes the canonical name
// long. Stripping that prefix and lowercasing gives the short form the CLI
// flags, the display, and docs/domain-model.md all use.

// prefix is the prefix the values of ed carry: the enum's name in
// SCREAMING_SNAKE with a trailing underscore, so Priority gives PRIORITY_
// and AssigneeKind gives ASSIGNEE_KIND_.
func prefix(ed protoreflect.EnumDescriptor) string {
	return screamingSnake(string(ed.Name())) + "_"
}

// screamingSnake renders a CamelCase identifier as SCREAMING_SNAKE.
func screamingSnake(name string) string {
	var b strings.Builder
	runes := []rune(name)
	for i, r := range runes {
		if unicode.IsUpper(r) && i > 0 {
			prev := runes[i-1]
			next := rune(0)
			if i+1 < len(runes) {
				next = runes[i+1]
			}
			if !unicode.IsUpper(prev) || unicode.IsLower(next) {
				b.WriteByte('_')
			}
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}

// Short is the short form of the value of ed named name: the canonical
// name minus the enum's prefix, lowercased. A name that does not carry the
// prefix, or whose short form would not decode back to the same value, is
// returned unchanged, so what Short emits is always something [Value]
// accepts.
func Short(ed protoreflect.EnumDescriptor, name string) string {
	if ed.Values().ByName(protoreflect.Name(name)) == nil {
		return name
	}
	short := strings.ToLower(strings.TrimPrefix(name, prefix(ed)))
	if v, ok := Value(ed, short); !ok || string(v.Name()) != name {
		return name
	}
	return short
}

// Value resolves one spelling of a value of ed: the canonical name
// (PRIORITY_P2), the name minus the enum's prefix (P2), or that lowercased
// (p2). Hyphens stand in for underscores, so no-longer-relevant reads as
// NO_LONGER_RELEVANT.
func Value(ed protoreflect.EnumDescriptor, spelling string) (protoreflect.EnumValueDescriptor, bool) {
	if spelling == "" {
		return nil, false
	}
	if v := ed.Values().ByName(protoreflect.Name(spelling)); v != nil {
		return v, true
	}
	key := strings.ToUpper(strings.ReplaceAll(spelling, "-", "_"))
	if v := ed.Values().ByName(protoreflect.Name(key)); v != nil {
		return v, true
	}
	if v := ed.Values().ByName(protoreflect.Name(prefix(ed) + key)); v != nil {
		return v, true
	}
	return nil, false
}

// EnumName is the short form of an enum value: p2 for PRIORITY_P2. A value
// outside the enum's range renders as its number.
func EnumName(e protoreflect.Enum) string {
	ed := e.Descriptor()
	v := ed.Values().ByNumber(e.Number())
	if v == nil {
		return strconv.Itoa(int(e.Number()))
	}
	return Short(ed, string(v.Name()))
}

// EnumNames lists the short forms of an enum's values, sorted, without the
// zero value: it is the unspecified one on every dagger enum and naming it
// is never how a caller asks for a default.
func EnumNames(ed protoreflect.EnumDescriptor) []string {
	names := make([]string, 0, ed.Values().Len())
	for i := range ed.Values().Len() {
		v := ed.Values().Get(i)
		if v.Number() == 0 {
			continue
		}
		names = append(names, Short(ed, string(v.Name())))
	}
	return slices.Sorted(slices.Values(names))
}
