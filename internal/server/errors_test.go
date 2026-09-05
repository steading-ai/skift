package server_test

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/dagger/internal/domain"
	"github.com/ivy/dagger/internal/policy"
	"github.com/ivy/dagger/internal/server"
)

var _ = Describe("Connect codes for domain errors", Label("unit"), func() {
	wrapped := func(err error) error {
		return &domain.OpError{Index: 2, Op: "add_edge", Err: fmt.Errorf("context: %w", err)}
	}

	DescribeTable("maps each typed domain error to its code, through any wrapping",
		func(err error, code connect.Code) {
			Expect(connect.CodeOf(server.ConnectError(err))).To(Equal(code))
			Expect(connect.CodeOf(server.ConnectError(wrapped(err)))).To(Equal(code))
		},
		Entry("a reference that resolved to nothing", &domain.NotFoundError{Ref: domain.Ref{Project: "x", Number: 7}, Missing: domain.MissingProject}, connect.CodeNotFound),
		Entry("a node id nobody holds", domain.ErrNodeNotFound, connect.CodeNotFound),
		Entry("a policy denial", &domain.Denial{Op: "create_node", Err: &policy.DeniedError{Action: "create_node", Reasons: []string{"no"}}}, connect.CodePermissionDenied),
		Entry("a cycle", &domain.CycleError{Path: []domain.Hop{{From: domain.NodeRef{Project: "p", Number: 1}, To: domain.NodeRef{Project: "p", Number: 2}, Via: domain.RelationBlocks}}}, connect.CodeFailedPrecondition),
		Entry("a lease that is not live", domain.ErrLeaseNotLive, connect.CodeFailedPrecondition),
		Entry("a lease held by somebody else", domain.ErrNotLeaseHolder, connect.CodeFailedPrecondition),
		Entry("a write to a held node carrying no lease", domain.ErrLeaseRequired, connect.CodeFailedPrecondition),
		Entry("a write carrying the wrong lease", domain.ErrLeaseMismatch, connect.CodeFailedPrecondition),
		Entry("an archived project", domain.ErrProjectArchived, connect.CodeFailedPrecondition),
		Entry("a transition the status forbids", domain.ErrStatusConflict, connect.CodeFailedPrecondition),
		Entry("a claim lost to contention", domain.ErrClaimContended, connect.CodeAborted),
		Entry("a ceiling exceeded", &domain.LimitError{Limit: domain.LimitOpsPerPatch, Max: 10, Value: 11}, connect.CodeResourceExhausted),
		Entry("a request id reused with different content", &domain.RequestConflictError{RequestID: "r"}, connect.CodeAlreadyExists),
		Entry("a malformed reference", &domain.ParseError{Input: "a b", Reason: "spaces"}, connect.CodeInvalidArgument),
		Entry("a missing request id", domain.ErrNoRequestID, connect.CodeInvalidArgument),
		Entry("a missing reason", domain.ErrReasonRequired, connect.CodeInvalidArgument),
		Entry("a missing actor", domain.ErrNoActor, connect.CodeUnauthenticated),
		Entry("a cancelled request", context.Canceled, connect.CodeCanceled),
		Entry("anything untyped", errors.New("disk on fire"), connect.CodeInternal),
	)

	It("keeps the domain's message, so a rejected patch says why", func() {
		cycle := &domain.CycleError{Path: []domain.Hop{
			{From: domain.NodeRef{Project: "p", Number: 1}, To: domain.NodeRef{Project: "p", Number: 2}, Via: domain.RelationBlocks},
			{From: domain.NodeRef{Project: "p", Number: 2}, To: domain.NodeRef{Project: "p", Number: 1}, Via: domain.RelationBlocks},
		}}
		err := server.ConnectError(wrapped(cycle))
		Expect(err.Error()).To(SatisfyAll(ContainSubstring("p#1"), ContainSubstring("p#2"), ContainSubstring("operation 2 (add_edge)")))
	})

	It("passes a Connect error through unchanged and nil as nil", func() {
		orig := connect.NewError(connect.CodeUnavailable, errors.New("later"))
		Expect(server.ConnectError(orig)).To(BeIdenticalTo(orig))
		Expect(server.ConnectError(nil)).To(BeNil())
	})
})
