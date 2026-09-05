package server_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/durationpb"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/gen/dagger/v1/daggerv1connect"
	"github.com/ivy/dagger/internal/domain"
	"github.com/ivy/dagger/internal/server"
	"github.com/ivy/dagger/internal/server/interceptor"
)

// The handler specs run the real handlers over the real domain against a
// spec-private database, reached through a unix socket with the principal
// interceptor in front: the in-process shape of `dg serve`. The spec
// process is the only client, so its uid is the one identity, and which
// principal that identity maps to is switched per call.

// myIdentity is the unix identity of this process, which SO_PEERCRED
// reports for every client the specs open.
var myIdentity = interceptor.Identity{Scheme: interceptor.SchemeUnix, Value: strconv.Itoa(os.Getuid())}

// ulidCounter feeds newID, so every id a spec mints is distinct within the
// process; each process has a database of its own.
var ulidCounter atomic.Int64

// newID mints a ULID-shaped id from a counter: digits only, so it is in
// the Crockford alphabet and passes the schema's is_ulid.
func newID() string {
	return fmt.Sprintf("01J%023d", ulidCounter.Add(1))
}

// switchLookup maps the spec's identity to whichever principal the spec
// is acting as right now.
type switchLookup struct {
	mu        sync.Mutex
	principal interceptor.Principal
}

func (l *switchLookup) LookupIdentity(_ context.Context, id interceptor.Identity) (interceptor.Principal, error) {
	if id != myIdentity {
		return interceptor.Principal{}, &interceptor.UnmappedIdentityError{Identity: id}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.principal, nil
}

func (l *switchLookup) set(p interceptor.Principal) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.principal = p
}

// fixture is one organization with a user and an agent principal, served.
type fixture struct {
	pool   *pgxpool.Pool
	run    *running
	lookup *switchLookup
	org    string
	user   interceptor.Principal
	agent  interceptor.Principal
}

// newFixture creates the organization and its principals directly, since
// no RPC creates either, and starts the server. Calls act as the user
// until [fixture.as] says otherwise.
func newFixture(ctx context.Context) *fixture {
	GinkgoHelper()
	pool := pg.Database(ctx)
	f := &fixture{pool: pool, org: newID()}
	f.user = interceptor.Principal{ID: newID(), OrgID: f.org, Kind: interceptor.KindUser}
	f.agent = interceptor.Principal{ID: newID(), OrgID: f.org, Kind: interceptor.KindAgent}
	_, err := pool.Exec(ctx, `INSERT INTO organizations (id, name) VALUES ($1, $2)`, f.org, "acme")
	Expect(err).NotTo(HaveOccurred())
	for _, p := range []interceptor.Principal{f.user, f.agent} {
		_, err := pool.Exec(ctx, `INSERT INTO principals (id, org_id, kind) VALUES ($1, $2, $3)`, p.ID, p.OrgID, string(p.Kind))
		Expect(err).NotTo(HaveOccurred())
	}

	svc := domain.NewService(pool, pol)
	f.lookup = &switchLookup{principal: f.user}
	f.run = start(server.Config{
		Interceptors: []connect.Interceptor{interceptor.NewPrincipal(f.lookup, interceptor.UnixResolver{})},
	}, server.Services{
		Graph: server.NewGraphService(svc),
		Work:  server.NewWorkService(svc),
	}, pool)
	return f
}

// as switches which principal the spec's calls are attributed to.
func (f *fixture) as(p interceptor.Principal) { f.lookup.set(p) }

func (f *fixture) graph() daggerv1connect.GraphServiceClient { return f.run.graph() }
func (f *fixture) work() daggerv1connect.WorkServiceClient   { return f.run.work() }

// project creates a project over the wire and returns it.
func (f *fixture) project(ctx context.Context, title string) *daggerv1.Project {
	GinkgoHelper()
	res, err := f.graph().CreateProject(ctx, connect.NewRequest(&daggerv1.CreateProjectRequest{RequestId: newID(), Title: title}))
	Expect(err).NotTo(HaveOccurred())
	return res.Msg.GetProject()
}

// apply sends one patch to project with a fresh request id.
func (f *fixture) apply(ctx context.Context, project string, ops ...*daggerv1.PatchOp) (*daggerv1.ApplyPatchResponse, error) {
	GinkgoHelper()
	return f.applyWith(ctx, &daggerv1.ApplyPatchRequest{RequestId: newID(), Project: project, Ops: ops})
}

func (f *fixture) applyWith(ctx context.Context, req *daggerv1.ApplyPatchRequest) (*daggerv1.ApplyPatchResponse, error) {
	GinkgoHelper()
	res, err := f.graph().ApplyPatch(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// mustApply is apply for fixtures, against the "platform" project every
// spec creates first.
func (f *fixture) mustApply(ctx context.Context, ops ...*daggerv1.PatchOp) *daggerv1.ApplyPatchResponse {
	GinkgoHelper()
	res, err := f.apply(ctx, "platform", ops...)
	Expect(err).NotTo(HaveOccurred())
	return res
}

// getNode reads one node by reference.
func (f *fixture) getNode(ctx context.Context, project, ref string) *daggerv1.GetNodeResponse {
	GinkgoHelper()
	res, err := f.graph().GetNode(ctx, connect.NewRequest(&daggerv1.GetNodeRequest{Project: project, Node: ref}))
	Expect(err).NotTo(HaveOccurred())
	return res.Msg
}

// claim claims the next ready node of project as the agent, with the
// given TTL, and returns the response.
func (f *fixture) claim(ctx context.Context, project string, ttl *durationpb.Duration) *daggerv1.ClaimNextResponse {
	GinkgoHelper()
	f.as(f.agent)
	defer f.as(f.user)
	res, err := f.work().ClaimNext(ctx, connect.NewRequest(&daggerv1.ClaimNextRequest{
		RequestId: newID(), Project: project, Ttl: ttl, Harness: "spec", AgentRef: "spec",
	}))
	Expect(err).NotTo(HaveOccurred())
	return res.Msg
}

// Operation builders.

func createOp(alias, title string) *daggerv1.PatchOp {
	return &daggerv1.PatchOp{Op: &daggerv1.PatchOp_CreateNode{CreateNode: &daggerv1.CreateNodeOp{Alias: alias, Title: title}}}
}

func createOpWith(c *daggerv1.CreateNodeOp) *daggerv1.PatchOp {
	return &daggerv1.PatchOp{Op: &daggerv1.PatchOp_CreateNode{CreateNode: c}}
}

func edgeOp(from, to string) *daggerv1.PatchOp {
	return &daggerv1.PatchOp{Op: &daggerv1.PatchOp_AddEdge{AddEdge: &daggerv1.AddEdgeOp{From: from, To: to}}}
}

func updateOp(u *daggerv1.UpdateNodeOp) *daggerv1.PatchOp {
	return &daggerv1.PatchOp{Op: &daggerv1.PatchOp_UpdateNode{UpdateNode: u}}
}

func completeOp(node, body string, artifacts ...*daggerv1.Artifact) *daggerv1.PatchOp {
	return &daggerv1.PatchOp{Op: &daggerv1.PatchOp_CompleteNode{CompleteNode: &daggerv1.CompleteNodeOp{
		Node: node, Result: &daggerv1.Result{Body: body, Artifacts: artifacts},
	}}}
}

func attachOp(node string, artifact *daggerv1.Artifact) *daggerv1.PatchOp {
	return &daggerv1.PatchOp{Op: &daggerv1.PatchOp_AttachArtifact{AttachArtifact: &daggerv1.AttachArtifactOp{Node: node, Artifact: artifact}}}
}

func commentOp(node, body string) *daggerv1.PatchOp {
	return &daggerv1.PatchOp{Op: &daggerv1.PatchOp_AddComment{AddComment: &daggerv1.AddCommentOp{Node: node, Body: body}}}
}

func textArtifact(name, text string) *daggerv1.Artifact {
	return &daggerv1.Artifact{Name: name, Parts: []*daggerv1.Part{{Content: &daggerv1.Part_Text{Text: &daggerv1.TextPart{Text: text}}}}}
}

// numbers renders nodes as their numbers, in order.
func numbers(nodes []*daggerv1.Node) []int64 {
	out := make([]int64, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.GetNumber())
	}
	return out
}
