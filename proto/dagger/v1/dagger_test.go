package daggerv1

import (
	"bufio"
	"bytes"
	"os"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// outputOnly is the marker a field comment opens with when the server
// assigns the value and ignores anything a request carries.
const outputOnly = "Output only."

// field is one field declaration and the comment above it.
type field struct {
	message string
	name    string
	comment string
}

// enumValue is one enumerator and its number.
type enumValue struct {
	enum   string
	name   string
	number string
}

// contract is the proto file reduced to what the specs assert over.
type contract struct {
	messages map[string]string // message name to its leading comment
	fields   []field
	enums    map[string][]enumValue
}

// Match against a single trimmed line of the proto.
var (
	messageRe   = regexp.MustCompile(`^message\s+(\w+)\s*\{`)
	enumRe      = regexp.MustCompile(`^enum\s+(\w+)\s*\{`)
	oneofRe     = regexp.MustCompile(`^oneof\s+\w+\s*\{`)
	fieldRe     = regexp.MustCompile(`^(?:repeated\s+|optional\s+)?[\w.]+\s+(\w+)\s*=\s*\d+\s*;`)
	enumValueRe = regexp.MustCompile(`^(\w+)\s*=\s*(\d+)\s*;`)
)

// parse reads a proto file with a scanner that understands exactly the
// grammar dagger.proto uses: line comments, messages, enums, oneofs, and
// one declaration per line. It does not need to be a full parser; buf lint
// owns syntax, and this only pairs declarations with their comments.
func parse(path string) contract {
	src, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())

	c := contract{messages: map[string]string{}, enums: map[string][]enumValue{}}
	var (
		comment []string
		scope   []string // enclosing declarations, innermost last
		message string   // innermost enclosing message
		enum    string   // enclosing enum, if any
	)
	take := func() string {
		s := strings.Join(comment, " ")
		comment = nil
		return s
	}
	sc := bufio.NewScanner(bytes.NewReader(src))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "//"):
			comment = append(comment, strings.TrimSpace(strings.TrimPrefix(line, "//")))
		case line == "":
			comment = nil
		case messageRe.MatchString(line):
			message = messageRe.FindStringSubmatch(line)[1]
			c.messages[message] = take()
			scope = append(scope, "message")
		case enumRe.MatchString(line):
			enum = enumRe.FindStringSubmatch(line)[1]
			c.enums[enum] = nil
			take()
			scope = append(scope, "enum")
		case oneofRe.MatchString(line):
			take()
			scope = append(scope, "oneof")
		case line == "}":
			Expect(scope).NotTo(BeEmpty(), "unbalanced brace")
			switch scope[len(scope)-1] {
			case "message":
				message = ""
			case "enum":
				enum = ""
			}
			scope = scope[:len(scope)-1]
		case enum != "" && enumValueRe.MatchString(line):
			m := enumValueRe.FindStringSubmatch(line)
			take()
			c.enums[enum] = append(c.enums[enum], enumValue{enum: enum, name: m[1], number: m[2]})
		case message != "" && fieldRe.MatchString(line):
			name := fieldRe.FindStringSubmatch(line)[1]
			c.fields = append(c.fields, field{message: message, name: name, comment: take()})
		default:
			comment = nil
		}
	}
	Expect(sc.Err()).NotTo(HaveOccurred())
	Expect(scope).To(BeEmpty(), "unbalanced brace")
	return c
}

// clientSettable is every field the server does not mark output only, which
// is exactly the set a request may populate.
func clientSettable(c contract) []string {
	var out []string
	for _, f := range c.fields {
		if !strings.HasPrefix(f.comment, outputOnly) {
			out = append(out, f.message+"."+f.name)
		}
	}
	return out
}

var _ = Describe("dagger.proto", func() {
	var c contract

	BeforeEach(func() {
		c = parse("dagger.proto")
		Expect(c.messages).NotTo(BeEmpty())
		Expect(c.fields).NotTo(BeEmpty())
		Expect(c.enums).NotTo(BeEmpty())
	})

	It("Provenance cannot be asserted by a client", func() {
		By("given the generated request messages")
		// Requests are built from these domain messages, so a field a
		// client may set here is a field it may set in a request.

		By("when I inspect every field a client may set")
		settable := clientSettable(c)
		Expect(settable).NotTo(BeEmpty())

		By("then no field sets principal, principal_kind, on_behalf_of, or identity_scheme")
		for _, name := range []string{"principal", "principal_kind", "on_behalf_of", "identity_scheme"} {
			Expect(settable).NotTo(ContainElement(HaveSuffix("."+name)), "a client may set %s", name)
		}
	})

	It("marks every actor field output-only", func() {
		// Beyond the four named in the scenario, every field that records
		// who did something is resolved from the transport.
		for _, f := range c.fields {
			switch f.name {
			case "principal", "principal_kind", "on_behalf_of", "identity_scheme",
				"holder", "author", "resolved_by":
				Expect(f.comment).To(HavePrefix(outputOnly), "%s.%s", f.message, f.name)
			}
		}
	})

	It("marks every server-assigned field output-only", func() {
		for _, f := range c.fields {
			switch f.name {
			case "id", "org_id", "project_id", "node_id", "number", "node_counter",
				"context_id", "created_at", "occurred_at", "expires_at", "archived_at",
				"blocked", "ready", "claimed", "human_touched":
				Expect(f.comment).To(HavePrefix(outputOnly), "%s.%s", f.message, f.name)
			}
		}
	})

	It("comments every message and field", func() {
		for name, comment := range c.messages {
			Expect(comment).NotTo(BeEmpty(), "message %s", name)
		}
		for _, f := range c.fields {
			Expect(f.comment).NotTo(BeEmpty(), "%s.%s", f.message, f.name)
		}
	})

	It("gives every enum an _UNSPECIFIED zero value", func() {
		for name, values := range c.enums {
			Expect(values).NotTo(BeEmpty(), "enum %s", name)
			Expect(values[0].number).To(Equal("0"), "enum %s", name)
			Expect(values[0].name).To(HaveSuffix("_UNSPECIFIED"), "enum %s", name)
		}
	})

	It("defines every message and enum the work order names", func() {
		for _, m := range []string{"Project", "Node", "Edge", "Lease", "Principal", "Result",
			"Artifact", "Part", "Comment", "Usage", "Event"} {
			Expect(c.messages).To(HaveKey(m))
		}
		for _, e := range []string{"Status", "Priority", "AssigneeKind", "PrincipalKind",
			"EdgeKind", "ReleaseReason", "DropReason"} {
			Expect(c.enums).To(HaveKey(e))
		}
	})
})
