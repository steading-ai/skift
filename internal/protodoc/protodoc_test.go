package protodoc_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/reflect/protoreflect"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/internal/protodoc"
)

var _ = Describe("Summaries of the proto's own comments", Label("unit"), func() {
	It("reads the comment on a message, a field, and an enum value", func() {
		By("given names taken from the descriptors rather than written out")
		op := (&daggerv1.UpdateNodeOp{}).ProtoReflect().Descriptor()
		node := op.Fields().ByName("node")
		reason := (daggerv1.DropReason_DROP_REASON_SUPERSEDED).Descriptor().
			Values().ByNumber(protoreflect.EnumNumber(daggerv1.DropReason_DROP_REASON_SUPERSEDED))

		By("then each resolves to the sentence written above it in the proto")
		Expect(protodoc.Summary(op.FullName())).To(Equal("UpdateNodeOp changes the mutable fields of a node."))
		Expect(protodoc.Summary(node.FullName())).To(Equal("Node reference of the node to change."))
		Expect(protodoc.Summary(reason.FullName())).To(Equal("DROP_REASON_SUPERSEDED means other work replaced this node."))
	})

	It("carries on past a lead sentence too short to say anything", func() {
		body := (&daggerv1.CreateNodeOp{}).ProtoReflect().Descriptor().Fields().ByName("body")
		Expect(protodoc.Summary(body.FullName())).To(Equal("Markdown. The prompt an agent receives."))
	})

	It("is empty for a name nothing documents", func() {
		Expect(protodoc.Summary("dagger.v1.NoSuchMessage")).To(BeEmpty())
	})

	It("documents every operation of a patch", func() {
		ops := (&daggerv1.PatchOp{}).ProtoReflect().Descriptor().Oneofs().ByName("op").Fields()
		for i := range ops.Len() {
			message := ops.Get(i).Message()
			Expect(protodoc.Summary(message.FullName())).NotTo(BeEmpty(), string(message.FullName()))
		}
	})
})
