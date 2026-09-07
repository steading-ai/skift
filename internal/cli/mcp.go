package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/internal/client"
	"github.com/ivy/dagger/internal/codec"
	"github.com/ivy/dagger/internal/logging"
	"github.com/ivy/dagger/internal/version"
)

// dg mcp is the CLI's client wrapped for a harness: an MCP server on stdio
// whose tools are the commands, taking the same references and sending the
// same requests. The protocol is JSON-RPC 2.0, one message per line, as
// the MCP stdio transport specifies:
// https://modelcontextprotocol.io/specification/2025-06-18/basic/transports
//
// What the wrapper adds is a lease lifetime bound to the process. A claim
// made through a tool is heartbeated in the background for as long as the
// server runs, and every lease still held when stdin closes or SIGTERM
// arrives is released with a reason. A process killed outright releases
// nothing, and the server's expiry returns its node to the ready set.

// mcpProtocolVersion is the newest protocol revision this server speaks.
// A client asking for an older one it also speaks gets that one back.
const mcpProtocolVersion = "2025-06-18"

var mcpProtocolVersions = []string{mcpProtocolVersion, "2025-03-26", "2024-11-05"}

// mcpShutdownTimeout bounds the releases made on the way out. A service
// that does not answer by then is left to expire the leases.
const mcpShutdownTimeout = 10 * time.Second

// mcpMinHeartbeat is the floor on the interval between heartbeats, so a
// lease with a tiny TTL does not turn into a busy loop.
const mcpMinHeartbeat = 200 * time.Millisecond

// JSON-RPC 2.0 error codes.
const (
	jsonrpcParseError     = -32700
	jsonrpcInvalidRequest = -32600
	jsonrpcMethodNotFound = -32601
	jsonrpcInvalidParams  = -32602
	jsonrpcInternalError  = -32603
)

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *jsonrpcError) Error() string { return e.Message }

func rpcError(code int, format string, args ...any) *jsonrpcError {
	return &jsonrpcError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// The MCP messages the server exchanges. Only the fields it reads or
// writes are modelled.

type mcpInitializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
	ClientInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"clientInfo"`
}

type mcpInitializeResult struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    mcpCapabilities `json:"capabilities"`
	ServerInfo      mcpServerInfo   `json:"serverInfo"`
	Instructions    string          `json:"instructions,omitempty"`
}

type mcpCapabilities struct {
	Tools struct {
		ListChanged bool `json:"listChanged"`
	} `json:"tools"`
}

type mcpServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type mcpToolInfo struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type mcpCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpCallResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

// mcpTool is one tool: its advertisement and the call behind it. call
// returns the wire response the tool relays, or an error the model reads.
type mcpTool struct {
	name        string
	description string
	schema      map[string]any
	call        func(ctx context.Context, args json.RawMessage) (proto.Message, error)
}

// leaseHold is a lease this process claimed and keeps alive.
type leaseHold struct {
	id string
	// ref is the held node's reference, which is how a patch response
	// names the nodes it touched.
	ref string
	// stop ends the heartbeat loop.
	stop context.CancelFunc
}

// mcpServer is one dg mcp process: the client, the tools over it, and the
// leases it holds.
type mcpServer struct {
	app *app
	log *slog.Logger

	in  io.Reader
	out io.Writer
	// outMu serialises responses; tool calls run concurrently.
	outMu sync.Mutex

	tools []*mcpTool

	// sessionRef is minted here, once per process, because the harness
	// never tells the server its own session id. Every lease this process
	// claims carries it, so the leases of one session are queryable
	// together. See the Placement and resume section of
	// docs/domain-model.md.
	sessionRef string
	// harness is what the client called itself at initialize, recorded on
	// each lease.
	harness string
	// agentRef is where the attempts run: this host, unless a claim says
	// otherwise.
	agentRef string

	// calls counts in-flight requests, drained before shutdown releases.
	calls sync.WaitGroup

	holdMu sync.Mutex
	held   map[string]*leaseHold
	// heartbeats counts running heartbeat loops.
	heartbeats sync.WaitGroup
}

func (a *app) mcpCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve the CLI as an MCP server over stdio",
		Long: `Serve dagger to a harness such as Claude Code as an MCP server on stdin and
stdout. Each tool is one CLI command: same node references, same requests,
same responses as JSON.

The transport is the CLI's: the unix socket by default, or the tailnet or
HTTPS address given with --address, $` + client.EnvAddress + `, or the config
file, so a laptop session reaches a service on another machine.

A lease claimed through a tool lives as long as this process. It is renewed
in the background, and released with a reason when stdin closes or SIGTERM
arrives. A process killed outright leaves its leases to expire on the
server, which returns their nodes to the ready set.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runMCP(cmd)
		},
	}
}

func (a *app) runMCP(cmd *cobra.Command) error {
	// Configuration and transport errors surface before the server
	// answers anything; the client itself dials lazily.
	if _, err := a.connect(); err != nil {
		return fmt.Errorf("dg mcp: %w", err)
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return a.newMCPServer(cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()).serve(ctx)
}

func (a *app) newMCPServer(in io.Reader, out, diag io.Writer) *mcpServer {
	hostname, _ := os.Hostname()
	s := &mcpServer{
		app:        a,
		log:        logging.New(diag, slog.LevelInfo),
		in:         in,
		out:        out,
		sessionRef: client.NewRequestID(),
		harness:    "mcp",
		agentRef:   hostname,
		held:       map[string]*leaseHold{},
	}
	s.tools = s.toolset()
	return s
}

// serve runs until stdin closes or ctx is cancelled, then releases every
// lease still held.
func (s *mcpServer) serve(ctx context.Context) error {
	s.log.Info("dg mcp starting", "version", version.String(), "session_ref", s.sessionRef, "transport", s.app.client.Transport(), "target", s.app.client.Target())

	// In-flight calls share a context that ends with the session, so a
	// waiting claim stops when the harness goes away.
	callCtx, cancelCalls := context.WithCancel(ctx)
	defer cancelCalls()

	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		r := bufio.NewReader(s.in)
		for {
			line, err := r.ReadBytes('\n')
			if len(strings.TrimSpace(string(line))) > 0 {
				lines <- line
			}
			if err != nil {
				readErr <- err
				return
			}
		}
	}()

	how := ""
loop:
	for {
		select {
		case <-ctx.Done():
			how = "signal received"
			break loop
		case err := <-readErr:
			how = "stdin closed"
			if !errors.Is(err, io.EOF) {
				s.log.Error("read stdin", "error", err)
			}
			break loop
		case line := <-lines:
			s.dispatch(callCtx, line)
		}
	}

	s.log.Info("dg mcp shutting down", "cause", how)
	cancelCalls()
	s.calls.Wait()
	s.releaseAll("dg mcp: session ended before the node was completed (" + how + ")")
	return nil
}

// dispatch parses one line and answers it. Requests run concurrently so
// that a claim waiting for work does not stall a search.
func (s *mcpServer) dispatch(ctx context.Context, line []byte) {
	var req jsonrpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		s.reply(nil, nil, rpcError(jsonrpcParseError, "parse error: %v", err))
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		s.reply(req.ID, nil, rpcError(jsonrpcInvalidRequest, "invalid request: jsonrpc must be \"2.0\" and method must be set"))
		return
	}
	// A notification carries no id and gets no reply.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return
	}
	s.calls.Add(1)
	go func() {
		defer s.calls.Done()
		result, err := s.handle(ctx, req)
		s.reply(req.ID, result, err)
	}()
}

// reply writes one response. A nil id answers a message that could not be
// parsed, which JSON-RPC requires to carry a null id.
func (s *mcpServer) reply(id json.RawMessage, result any, err *jsonrpcError) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	res := jsonrpcResponse{JSONRPC: "2.0", ID: id, Result: result, Error: err}
	raw, marshalErr := json.Marshal(res)
	if marshalErr != nil {
		raw, _ = json.Marshal(jsonrpcResponse{JSONRPC: "2.0", ID: id, Error: rpcError(jsonrpcInternalError, "encode response: %v", marshalErr)})
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if _, err := s.out.Write(append(raw, '\n')); err != nil {
		s.log.Error("write stdout", "error", err)
	}
}

func (s *mcpServer) handle(ctx context.Context, req jsonrpcRequest) (any, *jsonrpcError) {
	switch req.Method {
	case "initialize":
		return s.initialize(req.Params)
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		infos := make([]mcpToolInfo, 0, len(s.tools))
		for _, t := range s.tools {
			infos = append(infos, mcpToolInfo{Name: t.name, Description: t.description, InputSchema: t.schema})
		}
		return map[string]any{"tools": infos}, nil
	case "tools/call":
		var params mcpCallParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, rpcError(jsonrpcInvalidParams, "tools/call: %v", err)
		}
		for _, t := range s.tools {
			if t.name == params.Name {
				return s.callTool(ctx, t, params.Arguments), nil
			}
		}
		return nil, rpcError(jsonrpcInvalidParams, "unknown tool %q; call tools/list for the tools this server offers", params.Name)
	default:
		return nil, rpcError(jsonrpcMethodNotFound, "method %q is not supported", req.Method)
	}
}

func (s *mcpServer) initialize(raw json.RawMessage) (any, *jsonrpcError) {
	var params mcpInitializeParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, rpcError(jsonrpcInvalidParams, "initialize: %v", err)
		}
	}
	if name := strings.TrimSpace(params.ClientInfo.Name); name != "" {
		s.harness = name
	}
	protocol := mcpProtocolVersion
	for _, v := range mcpProtocolVersions {
		if v == params.ProtocolVersion {
			protocol = v
		}
	}
	cfg, _ := s.app.config()
	project := cfg.Project
	if project == "" {
		project = "(none: call project_list for the slugs you may read, then qualify references as slug#7 or pass project)"
	}
	return mcpInitializeResult{
		ProtocolVersion: protocol,
		ServerInfo:      mcpServerInfo{Name: "dg", Version: version.String()},
		Instructions: fmt.Sprintf(`dagger is a project management system: a graph of work whose nodes become
ready when their dependencies are done. Current project: %s. Session: %s.

Typical flow: ready or search to find work, show_node and explain to read
it, claim to take it under a lease, then complete with a result. Leases
claimed here are renewed automatically and released with reason "crash"
if this session ends before completing; a hard kill leaves them to expire.
Node references are "#7" in the current project, "slug#7" in another
project, or "org/slug#7" fully qualified; project_list names the projects
you may read and the slugs they take.

Priority is p0 to p4, p2 the default: advisory ordering within a project's
ready set, never a gate on what may be worked.
kind is a free string dagger never interprets, task the default and bug,
spike and chore the rest of the conventional vocabulary.`, project, s.sessionRef),
	}, nil
}

// callTool runs one tool. Every failure, whether a bad argument or the
// service's answer, is a tool error the model reads and acts on.
func (s *mcpServer) callTool(ctx context.Context, t *mcpTool, args json.RawMessage) mcpCallResult {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	msg, err := t.call(ctx, args)
	if err != nil {
		return mcpCallResult{IsError: true, Content: []mcpContent{{Type: "text", Text: toolErrorText(t.name, err)}}}
	}
	raw, err := codec.MarshalIndent(msg)
	if err != nil {
		return mcpCallResult{IsError: true, Content: []mcpContent{{Type: "text", Text: t.name + ": encode response: " + err.Error()}}}
	}
	return mcpCallResult{Content: []mcpContent{{Type: "text", Text: string(raw)}}}
}

// toolErrorText renders an error for a model: the Connect code, the
// message, and for the codes a caller can do something about, what to do.
func toolErrorText(tool string, err error) string {
	e := ToError(err)
	var b strings.Builder
	fmt.Fprintf(&b, "%s failed (%s): %s", tool, e.Code, e.Message)
	switch codeOf(e.Code) {
	case connect.CodeInvalidArgument:
		b.WriteString("\nCheck the arguments against the tool description and try again.")
	case connect.CodeFailedPrecondition:
		b.WriteString("\nThe graph or lease is not in the state this needs. Use show_node or explain to see its current state; a lease that is no longer live must be claimed again.")
	case connect.CodeNotFound:
		b.WriteString("\nNothing matched that reference. References are \"#7\", \"slug#7\", or \"org/slug#7\"; search finds nodes by text.")
	case connect.CodePermissionDenied:
		b.WriteString("\nThe caller is not allowed to do this; a person must grant access or do it themselves.")
	case connect.CodeUnavailable, connect.CodeUnknown:
		b.WriteString("\nThe service could not be reached. Check that it is running and that the transport (socket or address) is right.")
	}
	for _, d := range e.Details {
		fmt.Fprintf(&b, "\ndetail: %s", d)
	}
	return b.String()
}

// codeOf maps a Connect code name back to the code; unknown names are
// CodeUnknown.
func codeOf(name string) connect.Code {
	var c connect.Code
	if err := c.UnmarshalText([]byte(name)); err != nil {
		return connect.CodeUnknown
	}
	return c
}

// Lease lifetime.

// hold records a lease claimed on node and starts renewing it.
func (s *mcpServer) hold(lease *daggerv1.Lease, node *daggerv1.Node) {
	ctx, stop := context.WithCancel(context.Background())
	h := &leaseHold{id: lease.GetId(), ref: node.GetReference(), stop: stop}
	s.holdMu.Lock()
	s.held[h.id] = h
	s.holdMu.Unlock()
	s.heartbeats.Add(1)
	go func() {
		defer s.heartbeats.Done()
		s.keepAlive(ctx, h, lease.GetExpiresAt().AsTime())
	}()
}

// forget stops renewing a lease. It is called when this process ends the
// lease itself, and when the server reports it gone.
func (s *mcpServer) forget(id string) {
	s.holdMu.Lock()
	h, ok := s.held[id]
	delete(s.held, id)
	s.holdMu.Unlock()
	if ok {
		h.stop()
	}
}

// heldLeases lists the leases this process holds.
func (s *mcpServer) heldLeases() []*leaseHold {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	out := make([]*leaseHold, 0, len(s.held))
	for _, h := range s.held {
		out = append(out, h)
	}
	return out
}

// keepAlive heartbeats a lease at a third of its remaining life until
// stopped or until the server says the lease is no longer live.
func (s *mcpServer) keepAlive(ctx context.Context, h *leaseHold, expiresAt time.Time) {
	for {
		wait := max(time.Until(expiresAt)/3, mcpMinHeartbeat)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		res, err := s.app.client.Work.Heartbeat(ctx, connect.NewRequest(&daggerv1.HeartbeatRequest{
			RequestId: client.NewRequestID(), LeaseId: h.id,
		}))
		switch {
		case err == nil:
			expiresAt = res.Msg.GetLease().GetExpiresAt().AsTime()
		case ctx.Err() != nil:
			return
		case connect.CodeOf(err) == connect.CodeFailedPrecondition || connect.CodeOf(err) == connect.CodeNotFound:
			s.log.Warn("lease is no longer live; no longer renewing it", "lease_id", h.id, "error", err)
			s.forget(h.id)
			return
		default:
			// Transient: try again at the next interval, which is now
			// shorter as expiry approaches.
			s.log.Warn("heartbeat failed", "lease_id", h.id, "error", err)
		}
	}
}

// releaseAll releases every held lease as a crash: the attempt ended
// without completing its node, and an orchestrator reading the reason
// should treat it as one to retry.
func (s *mcpServer) releaseAll(detail string) {
	held := s.heldLeases()
	for _, h := range held {
		s.forget(h.id)
	}
	s.heartbeats.Wait()
	if len(held) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpShutdownTimeout)
	defer cancel()
	for _, h := range held {
		_, err := s.app.client.Work.Release(ctx, connect.NewRequest(&daggerv1.ReleaseRequest{
			RequestId: client.NewRequestID(), LeaseId: h.id,
			Reason: daggerv1.ReleaseReason_RELEASE_REASON_CRASH, Detail: detail,
		}))
		if err != nil {
			s.log.Error("release lease on shutdown; it will expire instead", "lease_id", h.id, "error", err)
			continue
		}
		s.log.Info("released lease on shutdown", "lease_id", h.id)
	}
}
