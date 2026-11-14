package interceptor_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/gen/skift/v1/skiftv1connect"
	"github.com/steading-ai/skift/internal/server"
	"github.com/steading-ai/skift/internal/server/interceptor"
)

type contextGraph struct {
	skiftv1connect.UnimplementedGraphServiceHandler
	call func(context.Context) error
}

func (g contextGraph) GetProject(ctx context.Context, _ *connect.Request[skiftv1.GetProjectRequest]) (*connect.Response[skiftv1.GetProjectResponse], error) {
	if err := g.call(ctx); err != nil {
		return nil, err
	}
	return connect.NewResponse(&skiftv1.GetProjectResponse{}), nil
}

var _ = Describe("Authenticated in-process Connect dispatch", Label("unit"), func() {
	var lookup interceptor.StaticLookup
	var resolver interceptor.TailscaleResolver
	BeforeEach(func() {
		lookup = interceptor.StaticLookup{
			{Scheme: interceptor.SchemeTailscale, Value: "node-a"}: alice,
			{Scheme: interceptor.SchemeTailscale, Value: "node-b"}: {ID: "bob", OrgID: "other-org", Kind: interceptor.KindUser},
		}
		resolver = interceptor.TailscaleResolver{WhoIs: interceptor.WhoIsFunc(func(_ context.Context, peer string) (string, error) {
			switch peer {
			case "a":
				return "node-a", nil
			case "b":
				return "node-b", nil
			case "failed":
				return "", errors.New("WhoIs failed")
			default:
				return "unmapped", nil
			}
		})}
	})
	It("keeps concurrent callers distinct and reauthenticates inside generated server handlers", func(ctx SpecContext) {
		graph := newRecordingGraph()
		var specs []connect.Spec
		var mu sync.Mutex
		record := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				mu.Lock()
				specs = append(specs, req.Spec())
				mu.Unlock()
				return next(ctx, req)
			}
		})
		srv := server.New(server.Config{Interceptors: []connect.Interceptor{interceptor.NewPrincipal(lookup, resolver), record}}, server.Services{Graph: graph}, nil)
		client := skiftv1connect.NewGraphServiceClient(srv.InProcessClient(), baseURL)
		var wg sync.WaitGroup
		for _, peer := range []string{"a", "b"} {
			wg.Go(func() {
				defer GinkgoRecover()
				// Even an already-attributed context cannot override the transport.
				original := interceptor.WithCaller(interceptor.WithTailnetPeer(ctx, peer), interceptor.Principal{ID: "spoof"}, interceptor.Identity{})
				req := connect.NewRequest(&skiftv1.GetProjectRequest{})
				req.Header().Set("X-Forwarded-For", "b")
				req.Header().Set("X-Skift-Principal", "spoof")
				_, err := client.GetProject(original, req)
				Expect(err).NotTo(HaveOccurred())
			})
		}
		wg.Wait()
		var got []interceptor.Principal
		for range 2 {
			var seen attribution
			Expect(graph.seen).To(Receive(&seen))
			got = append(got, seen.principal)
			Expect(seen.identity.Scheme).To(Equal(interceptor.SchemeTailscale))
		}
		Expect(got).To(ConsistOf(alice, lookup[interceptor.Identity{Scheme: interceptor.SchemeTailscale, Value: "node-b"}]))
		Expect(specs).To(HaveLen(2))
		for _, spec := range specs {
			Expect(spec.IsClient).To(BeFalse())
		}
	})
	It("authenticates discovery and listing and rejects missing, unmapped, or failed identities before dispatch", func(ctx SpecContext) {
		auth := interceptor.NewAuthenticator(lookup, resolver)
		for _, path := range []string{"/discover", "/list"} {
			for _, peer := range []string{"", "unmapped", "failed", "a"} {
				called := false
				h := auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					called = true
					p, ok := interceptor.PrincipalFromContext(r.Context())
					Expect(ok).To(BeTrue())
					Expect(p).To(Equal(alice))
					w.WriteHeader(http.StatusOK)
				}))
				req := httptest.NewRequest(http.MethodPost, path, nil).WithContext(ctx)
				if peer != "" {
					req = req.WithContext(interceptor.WithTailnetPeer(ctx, peer))
				}
				req.Header.Set("X-Skift-Principal", alice.ID)
				req.Header.Set("Forwarded", "for=a")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if peer == "a" {
					Expect(w.Code).To(Equal(200))
					Expect(called).To(BeTrue())
				} else {
					Expect(w.Code).To(Equal(401))
					Expect(called).To(BeFalse())
				}
			}
		}
	})
	It("rejects unauthenticated tool RPCs despite spoofed headers", func(ctx SpecContext) {
		graph := newRecordingGraph()
		srv := server.New(server.Config{Interceptors: []connect.Interceptor{interceptor.NewPrincipal(lookup, resolver)}}, server.Services{Graph: graph}, nil)
		client := skiftv1connect.NewGraphServiceClient(srv.InProcessClient(), baseURL)
		for _, peer := range []string{"", "unmapped", "failed"} {
			callCtx := context.Context(ctx)
			if peer != "" {
				callCtx = interceptor.WithTailnetPeer(ctx, peer)
			}
			req := connect.NewRequest(&skiftv1.GetProjectRequest{})
			req.Header().Set("X-Skift-Principal", alice.ID)
			_, err := client.GetProject(callCtx, req)
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
		}
		Expect(graph.seen).NotTo(Receive())
	})
	It("preserves deadlines and cancellation through a waiting generated handler", func(ctx SpecContext) {
		started := make(chan struct{})
		deadline := time.Now().Add(time.Minute)
		callCtx, cancel := context.WithDeadline(interceptor.WithTailnetPeer(ctx, "a"), deadline)
		defer cancel()
		srv := server.New(server.Config{Interceptors: []connect.Interceptor{interceptor.NewPrincipal(lookup, resolver)}}, server.Services{Graph: contextGraph{call: func(ctx context.Context) error {
			d, ok := ctx.Deadline()
			Expect(ok).To(BeTrue())
			Expect(d).To(BeTemporally("~", deadline, time.Millisecond))
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}}}, nil)
		done := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			_, err := skiftv1connect.NewGraphServiceClient(srv.InProcessClient(), baseURL).GetProject(callCtx, connect.NewRequest(&skiftv1.GetProjectRequest{}))
			done <- err
		}()
		Eventually(started).Should(BeClosed())
		cancel()
		Eventually(done).Should(Receive(HaveOccurred()))
	})
	It("cannot dispatch back into MCP, health, or streaming events", func(ctx SpecContext) {
		srv := server.New(server.Config{}, server.Services{}, nil)
		for _, path := range []string{"/mcp", server.HealthPath, skiftv1connect.EventServiceWatchEventsProcedure} {
			req := httptest.NewRequest(http.MethodPost, baseURL+path, nil).WithContext(ctx)
			res, err := srv.InProcessClient().Do(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.StatusCode).To(Equal(http.StatusNotFound))
			Expect(res.Body.Close()).To(Succeed())
		}
	})
})
