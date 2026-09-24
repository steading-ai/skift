package interceptor_test

import (
	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/gen/dagger/v1/daggerv1connect"
	"github.com/ivy/dagger/internal/server"
	"github.com/ivy/dagger/internal/server/interceptor"
	"github.com/ivy/dagger/internal/testsupport"
)

var _ = Describe("Authorization at the interceptor", Label("unit"), func() {
	It("The interceptor no longer attempts resource-level authorization", func(ctx SpecContext) {
		testsupport.RequirePeerCredentials()
		By("given an interceptor built from an identity lookup alone, with no policy and no store")
		lookup := interceptor.StaticLookup{myIdentity: alice}
		graph := newRecordingGraph()
		client := serve(lookup, server.Services{Graph: graph}, interceptor.UnixResolver{})

		By("when an agent calls a method naming a project the default policy gives it no rights over")
		_, err := daggerv1connect.NewGraphServiceClient(client, baseURL).
			GetProject(ctx, connect.NewRequest(&daggerv1.GetProjectRequest{Project: "finance"}))

		By("then the interceptor admits the call, attributed, and leaves the resource decision to the domain")
		Expect(err).NotTo(HaveOccurred())
		var seen attribution
		Expect(graph.seen).To(Receive(&seen))
		Expect(seen.ok).To(BeTrue())
		Expect(seen.principal).To(Equal(alice))
		Expect(seen.principal.Kind).To(Equal(interceptor.KindAgent))
	})
})
