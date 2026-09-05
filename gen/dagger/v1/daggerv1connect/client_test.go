package daggerv1connect_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/gen/dagger/v1/daggerv1connect"
)

// serveUnix serves the handler on a unix socket in a scratch directory and
// returns a dialer bound to it. The server stops when the spec ends.
func serveUnix(handler http.Handler) func(context.Context, string, string) (net.Conn, error) {
	path := filepath.Join(GinkgoT().TempDir(), "dg.sock")
	listener, err := net.Listen("unix", path)
	Expect(err).NotTo(HaveOccurred())

	server := &http.Server{Handler: handler}
	go func() {
		defer GinkgoRecover()
		if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			Expect(err).NotTo(HaveOccurred())
		}
	}()
	DeferCleanup(server.Close)

	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", path)
	}
}

var _ = Describe("Generated Connect client", Label("unit"), func() {
	It("compiles against a unix socket transport", func() {
		By("given a GraphService handler listening on a unix socket")
		path, handler := daggerv1connect.NewGraphServiceHandler(daggerv1connect.UnimplementedGraphServiceHandler{})
		mux := http.NewServeMux()
		mux.Handle(path, handler)
		dial := serveUnix(mux)

		By("when a client built over that socket calls an RPC")
		// The URL's host is a placeholder: every connection goes through
		// the unix dialer, so the name never resolves.
		httpClient := &http.Client{Transport: &http.Transport{DialContext: dial}}
		client := daggerv1connect.NewGraphServiceClient(httpClient, "http://dagger")
		_, err := client.GetProject(context.Background(), connect.NewRequest(&daggerv1.GetProjectRequest{}))

		By("then the call reaches the handler and comes back as a Connect error")
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnimplemented))
	})
})
