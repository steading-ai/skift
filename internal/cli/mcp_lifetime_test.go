package cli

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/timestamppb"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/gen/skift/v1/skiftv1connect"
	"github.com/steading-ai/skift/internal/client"
)

type lifetimeWork struct {
	skiftv1connect.WorkServiceClient
	releases chan string
	claims   chan string
	lease    *skiftv1.Lease
	node     *skiftv1.Node
}

func (w lifetimeWork) Release(_ context.Context, r *connect.Request[skiftv1.ReleaseRequest]) (*connect.Response[skiftv1.ReleaseResponse], error) {
	w.releases <- r.Msg.GetLeaseId()
	return connect.NewResponse(&skiftv1.ReleaseResponse{}), nil
}

func (w lifetimeWork) ClaimNext(_ context.Context, r *connect.Request[skiftv1.ClaimNextRequest]) (*connect.Response[skiftv1.ClaimNextResponse], error) {
	w.claims <- r.Msg.GetRequestId()
	return connect.NewResponse(&skiftv1.ClaimNextResponse{Lease: w.lease, Node: w.node}), nil
}

var _ = Describe("stdio retried claim lifetime", Label("unit"), func() {
	It("tracks one renewer when a successful claim retry returns the same lease, then shuts down", func() {
		releases := make(chan string, 2)

		claims := make(chan string, 2)
		lease := &skiftv1.Lease{Id: "same-lease", ExpiresAt: timestamppb.New(time.Now().Add(time.Hour))}
		node := &skiftv1.Node{Reference: "platform#1"}
		s := &mcpServer{app: &app{projects: []string{"platform"}, client: &client.Client{Work: lifetimeWork{releases: releases, claims: claims, lease: lease, node: node}}}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), held: map[string]*leaseHold{}}
		s.tools = s.toolset()
		var claim *mcpTool
		for _, t := range s.tools {
			if t.name == "claim" {
				claim = t
			}
		}
		Expect(claim).NotTo(BeNil())
		args := json.RawMessage(`{"request_id":"retry-claim"}`)
		Expect(s.callTool(context.Background(), claim, args).IsError).To(BeFalse())
		first := s.heldLeases()[0]
		Expect(s.callTool(context.Background(), claim, args).IsError).To(BeFalse())
		Expect(claims).To(Receive(Equal("retry-claim")))
		Expect(claims).To(Receive(Equal("retry-claim")))
		Expect(s.heldLeases()).To(HaveLen(1))
		Expect(s.heldLeases()[0]).To(BeIdenticalTo(first))
		done := make(chan struct{})
		go func() { s.releaseAll("session ended"); close(done) }()
		Eventually(done).Should(BeClosed())
		Expect(releases).To(Receive(Equal("same-lease")))
		Expect(releases).NotTo(Receive())
		Expect(s.heldLeases()).To(BeEmpty())
	})
})
