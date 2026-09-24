package server_test

import (
	"context"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/internal/testsupport"
)

// receive is the next message of stream, failing when it ends first.
func receive(stream *connect.ServerStreamForClient[skiftv1.WatchEventsResponse]) *skiftv1.WatchEventsResponse {
	GinkgoHelper()
	Expect(stream.Receive()).To(BeTrue(), "stream ended: %v", stream.Err())
	return stream.Msg()
}

// receiveEvent is the next message carrying an event.
func receiveEvent(stream *connect.ServerStreamForClient[skiftv1.WatchEventsResponse]) *skiftv1.WatchEventsResponse {
	GinkgoHelper()
	for {
		if msg := receive(stream); msg.GetEvent() != nil {
			return msg
		}
	}
}

var _ = Describe("EventService", Label(testsupport.DBLabel), func() {
	It("streams events with resumable cursors over the wire", func(ctx SpecContext) {
		f := newFixture(ctx)
		f.project(ctx, "Platform")
		sctx, cancel := context.WithCancel(ctx)
		defer cancel()
		stream, err := f.events().WatchEvents(sctx, connect.NewRequest(&skiftv1.WatchEventsRequest{Ops: []string{"create_node"}}))
		Expect(err).NotTo(HaveOccurred())

		By("a stream from now opens with a position and no event")
		opening := receive(stream)
		Expect(opening.GetEvent()).To(BeNil())
		Expect(opening.GetCursor()).NotTo(BeEmpty())

		By("each event arrives with the cursor to resume after it")
		res, err := f.applyWith(ctx, &skiftv1.ApplyPatchRequest{RequestId: newID(), Project: "platform", View: skiftv1.View_VIEW_FULL, Ops: []*skiftv1.PatchOp{createOp("a", "A"), createOp("b", "B")}})
		Expect(err).NotTo(HaveOccurred())
		first := receiveEvent(stream)
		Expect(first.GetEvent().GetId()).To(Equal(res.GetEvents()[0].GetId()))
		Expect(first.GetEvent().GetOp()).To(Equal("create_node"))
		Expect(first.GetEvent().GetPrincipal()).To(Equal(f.user.ID))
		Expect(first.GetCursor()).NotTo(BeEmpty())
		second := receiveEvent(stream)
		Expect(second.GetEvent().GetId()).To(Equal(res.GetEvents()[1].GetId()))
		cancel()

		By("resuming after the first event yields the second and nothing earlier")
		resumed, err := f.events().WatchEvents(ctx, connect.NewRequest(&skiftv1.WatchEventsRequest{Cursor: first.GetCursor()}))
		Expect(err).NotTo(HaveOccurred())
		defer resumed.Close() //nolint:errcheck // ends with the spec
		Expect(receiveEvent(resumed).GetEvent().GetId()).To(Equal(second.GetEvent().GetId()))
	})

	It("narrows the stream to one project", func(ctx SpecContext) {
		f := newFixture(ctx)
		f.project(ctx, "Platform")
		other := f.project(ctx, "Other")
		stream, err := f.events().WatchEvents(ctx, connect.NewRequest(&skiftv1.WatchEventsRequest{Project: other.GetSlug()}))
		Expect(err).NotTo(HaveOccurred())
		defer stream.Close() //nolint:errcheck // ends with the spec
		Expect(receive(stream).GetEvent()).To(BeNil())

		f.mustApply(ctx, createOp("a", "In platform"))
		res, err := f.applyWith(ctx, &skiftv1.ApplyPatchRequest{RequestId: newID(), Project: other.GetSlug(), View: skiftv1.View_VIEW_FULL, Ops: []*skiftv1.PatchOp{createOp("b", "In other")}})
		Expect(err).NotTo(HaveOccurred())
		Expect(receiveEvent(stream).GetEvent().GetId()).To(Equal(res.GetEvents()[0].GetId()))
	})

	It("refuses a malformed cursor as an invalid argument", func(ctx SpecContext) {
		f := newFixture(ctx)
		stream, err := f.events().WatchEvents(ctx, connect.NewRequest(&skiftv1.WatchEventsRequest{Cursor: "01J0000000000000000000000"}))
		Expect(err).NotTo(HaveOccurred())
		defer stream.Close() //nolint:errcheck // ends with the spec
		Expect(stream.Receive()).To(BeFalse())
		Expect(connect.CodeOf(stream.Err())).To(Equal(connect.CodeInvalidArgument))
	})

	It("ends the stream cleanly when the server drains", func(ctx SpecContext) {
		f := newFixture(ctx)
		stream, err := f.events().WatchEvents(ctx, connect.NewRequest(&skiftv1.WatchEventsRequest{}))
		Expect(err).NotTo(HaveOccurred())
		defer stream.Close() //nolint:errcheck // ends with the spec
		Expect(receive(stream).GetEvent()).To(BeNil())

		Expect(f.run.stop()).To(Succeed())
		Expect(stream.Receive()).To(BeFalse())
		Expect(stream.Err()).NotTo(HaveOccurred())
	})
})
