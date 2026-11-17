// Package mcptools adapts generated Connect clients into the shared MCP tool inventory.
// It does not resolve process configuration or own lease lifetimes.
package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/gen/skift/v1/skiftv1connect"
	"github.com/steading-ai/skift/internal/codec"
)

// HeldLease is the minimal process-local lease information needed for defaults.
type HeldLease struct{ ID, Ref string }

// LeaseHooks is supplied only by transports that own a lease lifetime.
// Native HTTP leaves it nil: a response lifetime is not a lease lifetime.
type LeaseHooks interface {
	Hold(*skiftv1.Lease, *skiftv1.Node)
	Forget(string)
	HeldLeases() []HeldLease
}

// Execution contains explicit caller defaults and authorized API access.
// New snapshots Projects. Hooks, if supplied, must support concurrent calls.
// Provenance fields annotate claims; they never grant identity or permission.
type Execution struct {
	Graph                         skiftv1connect.GraphServiceClient
	Work                          skiftv1connect.WorkServiceClient
	Projects                      []string
	Harness, AgentRef, SessionRef string
	Leases                        LeaseHooks
	// Draining returns the service stop signal from the original request context.
	// Nil leaves process-scoped stdio waiting behavior unchanged.
	Draining func(context.Context) <-chan struct{}
}

// Annotations are safety hints, not an authorization policy. Mutations mint
// request IDs when absent, so repeating arbitrary inputs is not idempotent.
type Annotations struct {
	ReadOnlyHint    bool `json:"readOnlyHint"`
	DestructiveHint bool `json:"destructiveHint"`
	IdempotentHint  bool `json:"idempotentHint"`
	OpenWorldHint   bool `json:"openWorldHint"`
}

type Tool struct {
	Name         string
	Description  string
	InputSchema  map[string]any
	OutputSchema map[string]any
	Annotations  Annotations
	call         func(context.Context, json.RawMessage) (proto.Message, error)
}

type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
type Result struct {
	Content           []Content      `json:"content"`
	StructuredContent map[string]any `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
}

// New builds an isolated tool inventory. No process environment is read.
func New(execution Execution) []*Tool {
	execution.Projects = append([]string(nil), execution.Projects...)
	tools := execution.toolset()
	for _, t := range tools {
		switch t.Name {
		case "search", "show_node", "node_list", "explain", "inputs", "ready", "project_list", "project_show":
			t.Annotations = Annotations{ReadOnlyHint: true, IdempotentHint: true}
		default:
			t.Annotations = Annotations{DestructiveHint: true}
		}
		// Every generated tool response is a JSON object. Nested wire schemas
		// vary with views; do not advertise requirements the projection omits.
		t.OutputSchema = map[string]any{"type": "object"}
	}
	return tools
}

// Invoke preserves the caller's context through the generated Connect client.
// Text and structured content are the same codec projection.
func (t *Tool) Invoke(ctx context.Context, args json.RawMessage) Result {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	msg, err := t.call(ctx, args)
	if err != nil {
		return Result{IsError: true, Content: []Content{{Type: "text", Text: ErrorText(t.Name, err)}}}
	}
	raw, err := codec.MarshalIndent(msg)
	if err != nil {
		return Result{IsError: true, Content: []Content{{Type: "text", Text: t.Name + ": encode response: " + err.Error()}}}
	}
	var structured map[string]any
	if err := json.Unmarshal(raw, &structured); err != nil {
		return Result{IsError: true, Content: []Content{{Type: "text", Text: t.Name + ": encode response: " + err.Error()}}}
	}
	return Result{Content: []Content{{Type: "text", Text: string(raw)}}, StructuredContent: structured}
}

func (s *Execution) currentProjects() []string {
	return append([]string(nil), s.Projects...)
}
func (s *Execution) hold(l *skiftv1.Lease, n *skiftv1.Node) {
	if s.Leases != nil {
		s.Leases.Hold(l, n)
	}
}
func (s *Execution) forget(id string) {
	if s.Leases != nil {
		s.Leases.Forget(id)
	}
}
func (s *Execution) heldLeases() []HeldLease {
	if s.Leases != nil {
		return s.Leases.HeldLeases()
	}
	return nil
}

// ErrorText renders transport and argument errors as actionable tool text.
func ErrorText(tool string, err error) string {
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
func codeOf(name string) connect.Code {
	var c connect.Code
	if err := c.UnmarshalText([]byte(name)); err != nil {
		return connect.CodeUnknown
	}
	return c
}
