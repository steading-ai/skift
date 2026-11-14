package mcptools_test

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/gen/skift/v1/skiftv1connect"
	"github.com/steading-ai/skift/internal/mcptools"
)

type graphStub struct {
	skiftv1connect.GraphServiceClient
	get   func(context.Context, *connect.Request[skiftv1.GetNodeRequest]) (*connect.Response[skiftv1.GetNodeResponse], error)
	patch func(context.Context, *connect.Request[skiftv1.ApplyPatchRequest]) (*connect.Response[skiftv1.ApplyPatchResponse], error)
}

func (g graphStub) GetNode(ctx context.Context, r *connect.Request[skiftv1.GetNodeRequest]) (*connect.Response[skiftv1.GetNodeResponse], error) {
	return g.get(ctx, r)
}
func (g graphStub) ApplyPatch(ctx context.Context, r *connect.Request[skiftv1.ApplyPatchRequest]) (*connect.Response[skiftv1.ApplyPatchResponse], error) {
	return g.patch(ctx, r)
}

type workStub struct {
	skiftv1connect.WorkServiceClient
	claim     func(context.Context, *connect.Request[skiftv1.ClaimNextRequest]) (*connect.Response[skiftv1.ClaimNextResponse], error)
	heartbeat func(context.Context, *connect.Request[skiftv1.HeartbeatRequest]) (*connect.Response[skiftv1.HeartbeatResponse], error)
}

func (w workStub) ClaimNext(ctx context.Context, r *connect.Request[skiftv1.ClaimNextRequest]) (*connect.Response[skiftv1.ClaimNextResponse], error) {
	return w.claim(ctx, r)
}
func (w workStub) Heartbeat(ctx context.Context, r *connect.Request[skiftv1.HeartbeatRequest]) (*connect.Response[skiftv1.HeartbeatResponse], error) {
	return w.heartbeat(ctx, r)
}

type hooks struct {
	mu   sync.Mutex
	held []mcptools.HeldLease
}

func (h *hooks) Hold(l *skiftv1.Lease, n *skiftv1.Node) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.held = append(h.held, mcptools.HeldLease{ID: l.GetId(), Ref: n.GetReference()})
}
func (h *hooks) Forget(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, l := range h.held {
		if l.ID == id {
			h.held = append(h.held[:i], h.held[i+1:]...)
			return
		}
	}
}
func (h *hooks) HeldLeases() []mcptools.HeldLease {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]mcptools.HeldLease(nil), h.held...)
}

func inventory(e mcptools.Execution) map[string]*mcptools.Tool {
	out := map[string]*mcptools.Tool{}
	for _, t := range mcptools.New(e) {
		out[t.Name] = t
	}
	return out
}

var _ = Describe("shared tool execution", func() {
	It("retains the stdio inventory, strict schemas, views, wire responses and retry keys", func() {
		g := graphStub{
			get: func(_ context.Context, r *connect.Request[skiftv1.GetNodeRequest]) (*connect.Response[skiftv1.GetNodeResponse], error) {
				Expect(r.Msg.GetProject()).To(Equal("platform"))
				Expect(r.Msg.GetNode()).To(Equal("#7"))
				Expect(r.Msg.GetView()).To(Equal(skiftv1.View_VIEW_FULL))
				return connect.NewResponse(&skiftv1.GetNodeResponse{Node: &skiftv1.Node{Title: "seven", Reference: "platform#7"}}), nil
			},
			patch: func(_ context.Context, r *connect.Request[skiftv1.ApplyPatchRequest]) (*connect.Response[skiftv1.ApplyPatchResponse], error) {
				Expect(r.Msg.GetProject()).To(Equal("platform"))
				Expect(r.Msg.GetRequestId()).To(Equal("retry-key"))
				Expect(r.Msg.GetView()).To(Equal(skiftv1.View_VIEW_REFS))
				Expect(r.Msg.GetOps()[0].GetCompleteNode().GetNode()).To(Equal("#7"))
				Expect(r.Msg.GetLeaseId()).To(Equal("lease"))
				return connect.NewResponse(&skiftv1.ApplyPatchResponse{Nodes: []*skiftv1.Node{{Reference: "platform#7", Status: skiftv1.Status_STATUS_DONE}}}), nil
			},
		}
		ts := inventory(mcptools.Execution{Graph: g, Projects: []string{"platform"}})
		Expect(ts).To(HaveLen(17))
		for _, t := range ts {
			Expect(t.InputSchema).To(HaveKeyWithValue("additionalProperties", false))
			Expect(t.OutputSchema).To(HaveKeyWithValue("type", "object"))
		}
		Expect(ts["node_list"].Annotations.ReadOnlyHint).To(BeTrue())
		Expect(ts["complete"].Annotations.DestructiveHint).To(BeTrue())
		Expect(ts["complete"].Annotations.IdempotentHint).To(BeFalse())
		res := ts["show_node"].Invoke(context.Background(), json.RawMessage(`{"ref":"#7"}`))
		Expect(res.IsError).To(BeFalse())
		var text map[string]any
		Expect(json.Unmarshal([]byte(res.Content[0].Text), &text)).To(Succeed())
		Expect(res.StructuredContent).To(Equal(text))
		Expect(text["node"]).To(HaveKeyWithValue("title", "seven"))
		res = ts["complete"].Invoke(context.Background(), json.RawMessage(`{"ref":"#7","result":"done","lease_id":"lease","request_id":"retry-key"}`))
		Expect(res.IsError).To(BeFalse())
		for _, raw := range []string{`{"ref":"#7","typo":true}`, `{"ref":7}`, `null`, `{} {}`} {
			res = ts["show_node"].Invoke(context.Background(), json.RawMessage(raw))
			Expect(res.IsError).To(BeTrue())
			Expect(res.Content[0].Text).To(ContainSubstring("invalid_argument"))
		}
	})

	It("isolates project defaults and lease bookkeeping when execution contexts overlap", func() {
		entered := make(chan string, 2)
		proceed := make(chan struct{})
		w := workStub{claim: func(ctx context.Context, r *connect.Request[skiftv1.ClaimNextRequest]) (*connect.Response[skiftv1.ClaimNextResponse], error) {
			project := r.Msg.GetFilter().GetProjects()[0]
			entered <- project
			select {
			case <-proceed:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return connect.NewResponse(&skiftv1.ClaimNextResponse{Lease: &skiftv1.Lease{Id: project + "-lease"}, Node: &skiftv1.Node{Reference: project + "#1"}}), nil
		}}
		ha, hb := &hooks{}, &hooks{}
		projects := []string{"alpha"}
		a := inventory(mcptools.Execution{Work: w, Projects: projects, Leases: ha})
		projects[0] = "changed"
		b := inventory(mcptools.Execution{Work: w, Projects: []string{"beta"}, Leases: hb})
		results := make(chan mcptools.Result, 2)
		for _, ts := range []map[string]*mcptools.Tool{a, b} {
			go func() { results <- ts["claim"].Invoke(context.Background(), json.RawMessage(`{}`)) }()
		}
		var first, second string
		Eventually(entered).Should(Receive(&first))
		Eventually(entered).Should(Receive(&second))
		Expect([]string{first, second}).To(ConsistOf("alpha", "beta"))
		close(proceed)
		for range 2 {
			var res mcptools.Result
			Eventually(results).Should(Receive(&res))
			Expect(res.IsError).To(BeFalse())
		}
		Expect(ha.HeldLeases()).To(Equal([]mcptools.HeldLease{{ID: "alpha-lease", Ref: "alpha#1"}}))
		Expect(hb.HeldLeases()).To(Equal([]mcptools.HeldLease{{ID: "beta-lease", Ref: "beta#1"}}))
	})

	It("uses no ambient project, provenance or lease default without explicit execution values", func() {
		w := workStub{claim: func(_ context.Context, r *connect.Request[skiftv1.ClaimNextRequest]) (*connect.Response[skiftv1.ClaimNextResponse], error) {
			Expect(r.Msg.GetFilter().GetProjects()).To(BeEmpty())
			Expect(r.Msg.GetHarness()).To(BeEmpty())
			Expect(r.Msg.GetAgentRef()).To(BeEmpty())
			Expect(r.Msg.GetSessionRef()).To(BeEmpty())
			Expect(r.Msg.GetRequestId()).To(Equal("retry"))
			return connect.NewResponse(&skiftv1.ClaimNextResponse{Lease: &skiftv1.Lease{Id: "committed"}}), nil
		}}
		ts := inventory(mcptools.Execution{Work: w})
		Expect(ts["claim"].Description).To(ContainSubstring("explicitly release"))
		Expect(ts["heartbeat"].Description).NotTo(ContainSubstring("automatically"))
		Expect(ts["claim"].Invoke(context.Background(), json.RawMessage(`{"request_id":"retry"}`)).IsError).To(BeFalse())
		Expect(ts["heartbeat"].Invoke(context.Background(), json.RawMessage(`{}`)).IsError).To(BeTrue())
		Expect(ts["show_node"].Invoke(context.Background(), json.RawMessage(`{"ref":"#1"}`)).IsError).To(BeTrue())
	})

	It("passes the original cancellation and deadline into waiting Connect calls", func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		entered := make(chan struct{})
		done := make(chan mcptools.Result, 1)
		w := workStub{claim: func(got context.Context, _ *connect.Request[skiftv1.ClaimNextRequest]) (*connect.Response[skiftv1.ClaimNextResponse], error) {
			Expect(got).To(BeIdenticalTo(ctx))
			close(entered)
			<-got.Done()
			return nil, connect.NewError(connect.CodeCanceled, got.Err())
		}}
		ts := inventory(mcptools.Execution{Work: w})
		go func() { defer GinkgoRecover(); done <- ts["claim"].Invoke(ctx, json.RawMessage(`{"wait":"1m"}`)) }()
		Eventually(entered).Should(BeClosed())
		cancel()
		var res mcptools.Result
		Eventually(done).Should(Receive(&res))
		Expect(res.IsError).To(BeTrue())
		Expect(res.Content[0].Text).To(ContainSubstring("canceled"))
	})
	It("stops before claim dispatch when the request service is already draining", func() {
		draining := make(chan struct{})
		close(draining)
		w := workStub{claim: func(context.Context, *connect.Request[skiftv1.ClaimNextRequest]) (*connect.Response[skiftv1.ClaimNextResponse], error) {
			Fail("draining claim dispatched")
			return nil, nil
		}}
		ctx := context.Background()
		ts := inventory(mcptools.Execution{Work: w, Draining: func(got context.Context) <-chan struct{} { Expect(got).To(BeIdenticalTo(ctx)); return draining }})
		Expect(ts["claim"].Invoke(ctx, json.RawMessage(`{"wait":"5m"}`)).IsError).To(BeFalse())
	})
	It("does not retry an empty waiting claim response after the service drain signal", func() {
		draining := make(chan struct{})
		calls := 0
		w := workStub{claim: func(context.Context, *connect.Request[skiftv1.ClaimNextRequest]) (*connect.Response[skiftv1.ClaimNextResponse], error) {
			calls++
			Expect(calls).To(Equal(1))
			close(draining)
			return connect.NewResponse(&skiftv1.ClaimNextResponse{}), nil
		}}
		ts := inventory(mcptools.Execution{Work: w, Draining: func(context.Context) <-chan struct{} { return draining }})
		Expect(ts["claim"].Invoke(context.Background(), json.RawMessage(`{"wait":"5m"}`)).IsError).To(BeFalse())
		Expect(calls).To(Equal(1))
	})
	It("preserves a committed claim returned while the service begins draining", func() {
		draining := make(chan struct{})
		w := workStub{claim: func(context.Context, *connect.Request[skiftv1.ClaimNextRequest]) (*connect.Response[skiftv1.ClaimNextResponse], error) {
			close(draining)
			return connect.NewResponse(&skiftv1.ClaimNextResponse{Node: &skiftv1.Node{Reference: "platform#1"}, Lease: &skiftv1.Lease{Id: "committed"}}), nil
		}}
		ts := inventory(mcptools.Execution{Work: w, Draining: func(context.Context) <-chan struct{} { return draining }})
		res := ts["claim"].Invoke(context.Background(), json.RawMessage(`{"wait":"5m"}`))
		Expect(res.IsError).To(BeFalse())
		Expect(res.StructuredContent["lease"]).To(HaveKeyWithValue("id", "committed"))
	})

})
