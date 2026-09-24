package acceptance_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
	"google.golang.org/protobuf/proto"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/internal/cli"
	"github.com/steading-ai/skift/internal/client"
	"github.com/steading-ai/skift/internal/codec"
	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/server"
	"github.com/steading-ai/skift/internal/server/interceptor"
	"github.com/steading-ai/skift/internal/testsupport"
)

// The deployment a scenario runs against mirrors docs/architecture.md:
//
//	host                                 laptop
//	  skift serve  ◄── unix socket ── skift, skift mcp   (agent: this uid)
//	            ◄── tailnet ────── skift, skift mcp   (user: a tailscale node)
//
// skift serve is the built binary on a unix socket, where SO_PEERCRED makes
// this process's uid the agent. A person needs a second identity, and the
// only other transport is the tailnet, so the laptop side is the same
// handler set served in-process over a loopback listener whose connections
// are marked as tsnet's are, with a WhoIs naming one node: the stand-in
// the server package's own tailnet specs use, since joining a real tailnet
// needs a coordination server. Everything below that listener is the
// production path, and every scenario reaches both through the binary.

// The organization the template database carries. No RPC creates an
// organization, a principal, or an identity, so the suite seeds them once
// when it migrates the template; that is the one write here that does not
// go through the service.
const (
	orgID   = "01J00000000000000000000001"
	agentID = "01J00000000000000000000002"
	userID  = "01J00000000000000000000003"
	// laptopNode is the tailscale node the user reaches the service from.
	laptopNode = "nLAPTOP01"
)

// migrateAndSeed brings the template to the current schema and gives it
// one organization with an agent reachable over the socket and a user
// reachable over the tailnet.
func migrateAndSeed(ctx context.Context, databaseURL string) error {
	if err := domain.Migrate(ctx, databaseURL); err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx) //nolint:errcheck // seeding session; nothing to flush
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO organizations (id, name) VALUES ($1, $2)`, []any{orgID, "acme"}},
		{`INSERT INTO principals (id, org_id, kind) VALUES ($1, $2, $3)`, []any{agentID, orgID, string(interceptor.KindAgent)}},
		{`INSERT INTO principals (id, org_id, kind) VALUES ($1, $2, $3)`, []any{userID, orgID, string(interceptor.KindUser)}},
		{`INSERT INTO principal_identities (principal_id, scheme, value) VALUES ($1, $2, $3)`, []any{agentID, interceptor.SchemeUnix, strconv.Itoa(os.Getuid())}},
		{`INSERT INTO principal_identities (principal_id, scheme, value) VALUES ($1, $2, $3)`, []any{userID, interceptor.SchemeTailscale, laptopNode}},
	}
	for _, s := range stmts {
		if _, err := conn.Exec(ctx, s.sql, s.args...); err != nil {
			return fmt.Errorf("seed template: %w", err)
		}
	}
	return nil
}

// loopbackTailnet is the laptop's way in: a TCP listener whose connections
// carry a tailnet peer, and a WhoIs that names the laptop for every one.
type loopbackTailnet struct {
	listener net.Listener
}

func (t *loopbackTailnet) Name() string { return "loopback-tailnet" }

func (t *loopbackTailnet) Listen() (net.Listener, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	t.listener = l
	return l, nil
}

func (t *loopbackTailnet) ConnContext(ctx context.Context, conn net.Conn) context.Context {
	return interceptor.TailnetConnContext(ctx, conn)
}

func (t *loopbackTailnet) Close() error { return nil }

func (t *loopbackTailnet) WhoIs(context.Context, string) (string, error) {
	return laptopNode, nil
}

// address is the URL a laptop client is configured with.
func (t *loopbackTailnet) address() string {
	return "http://" + t.listener.Addr().String()
}

// deployment is one scenario's service: a database of its own, skift serve
// on a socket, and the tailnet listener beside it.
type deployment struct {
	socket string
	// host is the agent, reaching skift serve over the socket.
	host face
	// laptop is the user, reaching the service over the tailnet.
	laptop face
}

// deploy starts a service over a fresh database. It is torn down when the
// scenario ends.
func deploy(ctx context.Context) *deployment {
	GinkgoHelper()
	pool := pg.Database(ctx)
	dir := GinkgoT().TempDir()
	d := &deployment{socket: filepath.Join(dir, "skift.sock")}

	By("starting skift serve on a unix socket over the scenario's database")
	cmd := exec.Command(binary, "serve",
		"--socket", d.socket,
		"--database-url", pool.Config().ConnString(),
		"--shutdown-timeout", "5s")
	session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		session.Terminate().Wait(10 * time.Second)
		session.Kill()
	})
	Eventually(func() error {
		_, err := os.Lstat(d.socket)
		return err
	}).Should(Succeed(), "skift serve never bound its socket")

	By("serving the same handlers on the tailnet")
	svc := domain.NewService(pool, pol)
	tn := &loopbackTailnet{}
	srv := server.New(server.Config{
		SocketPath:      filepath.Join(dir, "laptop.sock"),
		ShutdownTimeout: 5 * time.Second,
		Logger:          slog.New(slog.NewTextHandler(GinkgoWriter, nil)),
		Interceptors: []connect.Interceptor{interceptor.NewPrincipal(
			server.NewIdentityLookup(svc), interceptor.TailscaleResolver{WhoIs: tn},
		)},
		Transports: []server.Transport{tn},
	}, server.Services{
		Graph:  server.NewGraphService(svc),
		Work:   server.NewWorkService(svc),
		Events: server.NewEventService(svc),
	}, pool)
	Expect(srv.Listen()).To(Succeed())
	serveCtx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() {
		defer GinkgoRecover()
		stopped <- srv.Serve(serveCtx)
	}()
	DeferCleanup(func() {
		cancel()
		Eventually(stopped, 10*time.Second).Should(Receive(BeNil()))
	})

	env := cleanEnv(dir)
	d.host = face{name: "host", flags: []string{"--socket", d.socket}, env: env, unix: true}
	d.laptop = face{name: "laptop", flags: []string{"--address", tn.address()}, env: env}
	return d
}

// cleanEnv is the environment the binary runs under: the caller's, with
// every skift setting removed and the config directory pointed at scratch, so
// nothing configured on this machine reaches a scenario.
func cleanEnv(dir string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "SKIFT_") || strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "XDG_CONFIG_HOME="+filepath.Join(dir, "config"))
}

// face is one principal's way of running the binary: the connection flags
// that select a transport, and so an identity.
type face struct {
	name  string
	flags []string
	env   []string
	// unix is set on a face that reaches skift serve over its unix socket,
	// where the caller is whoever SO_PEERCRED says it is.
	unix bool
}

// command builds one invocation of the binary. A unix face skips the
// scenario where the platform cannot attribute a socket caller.
func (f face) command(args ...string) *exec.Cmd {
	if f.unix {
		testsupport.RequirePeerCredentials()
	}
	cmd := exec.Command(binary, append(append([]string{}, f.flags...), args...)...)
	cmd.Env = f.env
	return cmd
}

// run executes one command with stdin and returns what it wrote and its
// exit status.
func (f face) run(stdin string, args ...string) (stdout, stderr string, code int) {
	GinkgoHelper()
	cmd := f.command(args...)
	cmd.Stdin = strings.NewReader(stdin)
	session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	Eventually(session, 30*time.Second).Should(gexec.Exit(), "%s: skift %s did not exit", f.name, strings.Join(args, " "))
	return string(session.Out.Contents()), string(session.Err.Contents()), session.ExitCode()
}

// call runs a command that must succeed and decodes its JSON output into
// res.
func (f face) call(res proto.Message, args ...string) {
	GinkgoHelper()
	f.callWithInput(res, "", args...)
}

// callWithInput is call with stdin.
func (f face) callWithInput(res proto.Message, stdin string, args ...string) {
	GinkgoHelper()
	stdout, stderr, code := f.run(stdin, args...)
	Expect(code).To(BeZero(), "%s: skift %s failed: %s", f.name, strings.Join(args, " "), stderr)
	Expect(codec.Unmarshal([]byte(strings.TrimSpace(stdout)), res)).To(Succeed(), "%s: skift %s wrote %q", f.name, strings.Join(args, " "), stdout)
}

// fail runs a command that must fail and returns the error the CLI wrote
// to stderr.
func (f face) fail(stdin string, args ...string) cli.Error {
	GinkgoHelper()
	stdout, stderr, code := f.run(stdin, args...)
	Expect(code).NotTo(BeZero(), "%s: skift %s succeeded with %s", f.name, strings.Join(args, " "), stdout)
	var e cli.Error
	Expect(json.Unmarshal([]byte(strings.TrimSpace(stderr)), &e)).To(Succeed(), "%s: skift %s wrote %q to stderr", f.name, strings.Join(args, " "), stderr)
	return e
}

// patch applies req through "skift patch apply -".
func (f face) patch(req *skiftv1.ApplyPatchRequest, flags ...string) *skiftv1.ApplyPatchResponse {
	GinkgoHelper()
	raw, err := codec.Marshal(req)
	Expect(err).NotTo(HaveOccurred())
	res := &skiftv1.ApplyPatchResponse{}
	f.callWithInput(res, string(raw), append([]string{"patch", "apply", "-"}, flags...)...)
	return res
}

// failPatch applies req expecting the service to refuse it.
func (f face) failPatch(req *skiftv1.ApplyPatchRequest, flags ...string) cli.Error {
	GinkgoHelper()
	raw, err := codec.Marshal(req)
	Expect(err).NotTo(HaveOccurred())
	return f.fail(string(raw), append([]string{"patch", "apply", "-"}, flags...)...)
}

// createProject creates a project and returns it.
func (f face) createProject(title string) *skiftv1.Project {
	GinkgoHelper()
	res := &skiftv1.CreateProjectResponse{}
	f.call(res, "project", "create", title)
	return res.GetProject()
}

// platform is the project most scenarios work in, so the helpers below
// take it as read; a scenario in another project spells its commands out.
const platform = "platform"

// ready lists the ready set of platform with the given filter flags.
func (f face) ready(flags ...string) []*skiftv1.Node {
	GinkgoHelper()
	res := &skiftv1.ReadyResponse{}
	f.call(res, append([]string{"ready", "--project", platform}, flags...)...)
	return res.GetNodes()
}

// claim claims the next ready node of platform, or fails when none is.
func (f face) claim(flags ...string) *skiftv1.ClaimNextResponse {
	GinkgoHelper()
	res := &skiftv1.ClaimNextResponse{}
	f.call(res, append([]string{"claim", "--project", platform, "--harness", "acceptance"}, flags...)...)
	Expect(res.GetNode()).NotTo(BeNil(), "%s: nothing was ready to claim in %s", f.name, platform)
	return res
}

// explain asks why a node of platform is not ready.
func (f face) explain(ref string) *skiftv1.ExplainResponse {
	GinkgoHelper()
	res := &skiftv1.ExplainResponse{}
	f.call(res, "explain", "--project", platform, ref)
	return res
}

// show reads one node of platform in full.
func (f face) show(ref string) *skiftv1.GetNodeResponse {
	GinkgoHelper()
	res := &skiftv1.GetNodeResponse{}
	f.call(res, "node", "show", "--project", platform, ref)
	return res
}

// cursorNow opens the change stream of project from now and returns the
// position it opens at, so a later stream can replay everything from here.
func (f face) cursorNow(project string) string {
	GinkgoHelper()
	s := f.events(project, "")
	defer s.stop()
	opening := s.next()
	Expect(opening.GetEvent()).To(BeNil(), "a stream from now opens with a position alone")
	Expect(opening.GetCursor()).NotTo(BeEmpty())
	return opening.GetCursor()
}

// startPiped starts cmd under gexec, with its stdout also streamed to the
// returned reader, which ends when the process does. gexec owns
// cmd.Stdout, so a reader that wants lines as they are written gets them
// from a pipe gexec writes through rather than from cmd.StdoutPipe.
func startPiped(cmd *exec.Cmd, stdin io.Reader) (*gexec.Session, io.Reader) {
	GinkgoHelper()
	if stdin != nil {
		cmd.Stdin = stdin
	}
	pr, pw := io.Pipe()
	session, err := gexec.Start(cmd, pw, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	go func() {
		session.Wait(time.Hour)
		pw.Close() //nolint:errcheck // a pipe writer's Close never fails
	}()
	return session, pr
}

// eventStream is a running "skift events", one message per line.
type eventStream struct {
	session *gexec.Session
	lines   chan string
}

// events starts "skift events" on project from cursor, narrowed to ops when
// any are given.
func (f face) events(project, cursor string, ops ...string) *eventStream {
	GinkgoHelper()
	args := []string{"events"}
	if project != "" {
		args = append(args, "--project", project)
	}
	if cursor != "" {
		args = append(args, "--cursor", cursor)
	}
	for _, op := range ops {
		args = append(args, "--op", op)
	}
	session, stdout := startPiped(f.command(args...), nil)
	s := &eventStream{session: session, lines: make(chan string, 64)}
	go func() {
		defer GinkgoRecover()
		defer close(s.lines)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 1<<20), 1<<24)
		for scanner.Scan() {
			s.lines <- scanner.Text()
		}
	}()
	DeferCleanup(s.stop)
	return s
}

// next waits for the next message.
func (s *eventStream) next() *skiftv1.WatchEventsResponse {
	GinkgoHelper()
	var line string
	Eventually(s.lines, 15*time.Second).Should(Receive(&line), "no event arrived")
	res := &skiftv1.WatchEventsResponse{}
	Expect(codec.Unmarshal([]byte(line), res)).To(Succeed(), "skift events wrote %q", line)
	return res
}

// nextEvent waits for the next message that carries an event.
func (s *eventStream) nextEvent() *skiftv1.WatchEventsResponse {
	GinkgoHelper()
	for {
		if res := s.next(); res.GetEvent() != nil {
			return res
		}
	}
}

// quiet asserts that nothing more arrives for a while.
func (s *eventStream) quiet() {
	GinkgoHelper()
	Consistently(s.lines, 500*time.Millisecond).ShouldNot(Receive())
}

// stop interrupts the stream as a person would and waits for it to exit.
func (s *eventStream) stop() {
	if s.session.ExitCode() != -1 {
		return
	}
	s.session.Signal(syscall.SIGINT)
	Eventually(s.session, 10*time.Second).Should(gexec.Exit())
}

// mcpSession drives one "skift mcp" process over its stdio, as a harness
// does: JSON-RPC requests in, responses matched by id out.
type mcpSession struct {
	name    string
	stdin   io.WriteCloser
	session *gexec.Session

	mu      sync.Mutex
	pending map[int64]chan map[string]any
	next    int64
}

// mcp starts the MCP server for this face on project and performs the
// initialize handshake as a client calling itself claude-code.
func (f face) mcp(project string) *mcpSession {
	GinkgoHelper()
	cmd := f.command("mcp", "--project", project)
	stdin, err := cmd.StdinPipe()
	Expect(err).NotTo(HaveOccurred())
	session, stdout := startPiped(cmd, nil)
	m := &mcpSession{name: f.name, stdin: stdin, session: session, pending: map[int64]chan map[string]any{}}
	go func() {
		defer GinkgoRecover()
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 1<<20), 1<<24)
		for scanner.Scan() {
			var res struct {
				ID     int64          `json:"id"`
				Result map[string]any `json:"result"`
				Error  map[string]any `json:"error"`
			}
			Expect(json.Unmarshal(scanner.Bytes(), &res)).To(Succeed(), "stdout was not JSON-RPC: %s", scanner.Text())
			m.mu.Lock()
			ch := m.pending[res.ID]
			delete(m.pending, res.ID)
			m.mu.Unlock()
			if ch == nil {
				continue
			}
			if res.Error != nil {
				ch <- map[string]any{"error": res.Error}
			} else {
				ch <- res.Result
			}
		}
	}()
	DeferCleanup(m.close)

	res := m.send("initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "claude-code", "version": "1.0"},
	})
	Expect(res).NotTo(HaveKey("error"))
	m.notify("notifications/initialized")
	return m
}

// send writes one request and returns its result, or {"error": ...}.
func (m *mcpSession) send(method string, params any) map[string]any {
	GinkgoHelper()
	m.mu.Lock()
	m.next++
	id := m.next
	ch := make(chan map[string]any, 1)
	m.pending[id] = ch
	m.mu.Unlock()
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	Expect(err).NotTo(HaveOccurred())
	_, err = m.stdin.Write(append(raw, '\n'))
	Expect(err).NotTo(HaveOccurred())
	var res map[string]any
	Eventually(ch, 30*time.Second).Should(Receive(&res), "%s: no response to %s", m.name, method)
	return res
}

// notify writes one notification, which gets no reply.
func (m *mcpSession) notify(method string) {
	GinkgoHelper()
	_, err := fmt.Fprintf(m.stdin, `{"jsonrpc":"2.0","method":%q}`+"\n", method)
	Expect(err).NotTo(HaveOccurred())
}

// toolResult is one tools/call answer.
type toolResult struct {
	text    string
	isError bool
}

// into decodes the tool's text, the wire response as JSON, into res.
func (r toolResult) into(res proto.Message) {
	GinkgoHelper()
	Expect(r.isError).To(BeFalse(), "tool failed: %s", r.text)
	Expect(codec.Unmarshal([]byte(r.text), res)).To(Succeed(), "tool text was not a response: %s", r.text)
}

// call invokes one tool.
func (m *mcpSession) call(tool string, args any) toolResult {
	GinkgoHelper()
	res := m.send("tools/call", map[string]any{"name": tool, "arguments": args})
	Expect(res).NotTo(HaveKey("error"), "%s: tools/call %s was a protocol error: %v", m.name, tool, res["error"])
	content, _ := res["content"].([]any)
	Expect(content).To(HaveLen(1))
	first, _ := content[0].(map[string]any)
	Expect(first).To(HaveKeyWithValue("type", "text"))
	isError, _ := res["isError"].(bool)
	return toolResult{text: first["text"].(string), isError: isError}
}

// close ends the session as a harness does, by closing stdin, and waits
// for the process to exit. Leases still held are released on the way out.
func (m *mcpSession) close() {
	if m.session.ExitCode() != -1 {
		return
	}
	_ = m.stdin.Close()
	Eventually(m.session, 15*time.Second).Should(gexec.Exit(0), "%s: skift mcp did not exit cleanly", m.name)
}

// patchArgs renders req as the arguments of the apply_patch tool.
func patchArgs(req *skiftv1.ApplyPatchRequest) map[string]any {
	GinkgoHelper()
	raw, err := codec.Marshal(req)
	Expect(err).NotTo(HaveOccurred())
	var args map[string]any
	Expect(json.Unmarshal(raw, &args)).To(Succeed())
	return args
}

// Operation builders, for patches written in Go rather than by hand.

func createOp(alias, title string) *skiftv1.PatchOp {
	return createOpWith(&skiftv1.CreateNodeOp{Alias: alias, Title: title})
}

func createOpWith(c *skiftv1.CreateNodeOp) *skiftv1.PatchOp {
	return &skiftv1.PatchOp{Op: &skiftv1.PatchOp_CreateNode{CreateNode: c}}
}

func edgeOp(from, to string) *skiftv1.PatchOp {
	return &skiftv1.PatchOp{Op: &skiftv1.PatchOp_AddEdge{AddEdge: &skiftv1.AddEdgeOp{From: from, To: to}}}
}

func removeEdgeOp(from, to string) *skiftv1.PatchOp {
	return &skiftv1.PatchOp{Op: &skiftv1.PatchOp_RemoveEdge{RemoveEdge: &skiftv1.RemoveEdgeOp{From: from, To: to}}}
}

func completeOp(node, body string, artifacts ...*skiftv1.Artifact) *skiftv1.PatchOp {
	return &skiftv1.PatchOp{Op: &skiftv1.PatchOp_CompleteNode{CompleteNode: &skiftv1.CompleteNodeOp{
		Node: node, Result: &skiftv1.Result{Body: body, Artifacts: artifacts},
	}}}
}

func dropOp(node string) *skiftv1.PatchOp {
	return &skiftv1.PatchOp{Op: &skiftv1.PatchOp_DropNode{DropNode: &skiftv1.DropNodeOp{
		Node: node, Reason: skiftv1.DropReason_DROP_REASON_NO_LONGER_RELEVANT, Detail: "never mind",
	}}}
}

func updateOp(u *skiftv1.UpdateNodeOp) *skiftv1.PatchOp {
	return &skiftv1.PatchOp{Op: &skiftv1.PatchOp_UpdateNode{UpdateNode: u}}
}

func commentOp(node, body string) *skiftv1.PatchOp {
	return &skiftv1.PatchOp{Op: &skiftv1.PatchOp_AddComment{AddComment: &skiftv1.AddCommentOp{Node: node, Body: body}}}
}

func textArtifact(name, text string) *skiftv1.Artifact {
	return &skiftv1.Artifact{Name: name, Parts: []*skiftv1.Part{{Content: &skiftv1.Part_Text{Text: &skiftv1.TextPart{Text: text}}}}}
}

// request is a patch against project with a fresh request id, asking for
// the full view: the scenarios read the nodes and events it returns.
func request(project string, ops ...*skiftv1.PatchOp) *skiftv1.ApplyPatchRequest {
	return &skiftv1.ApplyPatchRequest{RequestId: client.NewRequestID(), Project: project, Ops: ops, View: skiftv1.View_VIEW_FULL}
}

// ref renders a node as the bare reference the CLI takes.
func ref(n *skiftv1.Node) string {
	return "#" + strconv.FormatInt(n.GetNumber(), 10)
}

// numbers renders nodes as their numbers.
func numbers(nodes []*skiftv1.Node) []int64 {
	out := make([]int64, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.GetNumber())
	}
	return out
}

// health reports what skift serve says about itself, for a scenario that
// wants to be sure the service, not a stale socket, is answering.
func (d *deployment) health(ctx context.Context) server.Health {
	GinkgoHelper()
	httpClient := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", d.socket)
		},
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://skift"+server.HealthPath, nil)
	Expect(err).NotTo(HaveOccurred())
	res, err := httpClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer res.Body.Close() //nolint:errcheck // response body of a finished read
	var body server.Health
	Expect(json.NewDecoder(res.Body).Decode(&body)).To(Succeed())
	return body
}
