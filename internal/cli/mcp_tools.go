package cli

import (
	"context"
	"encoding/json"
	"strings"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/internal/mcptools"
)

func (s *mcpServer) toolset() []*mcpTool {
	projects, err := s.app.currentProjects()
	tools := mcptools.New(mcptools.Execution{
		Graph: s.app.client.Graph, Work: s.app.client.Work, Projects: projects,
		Harness: s.harness, AgentRef: s.agentRef, SessionRef: s.sessionRef, Leases: stdioLeaseHooks{s},
	})
	out := make([]*mcpTool, 0, len(tools))
	for _, t := range tools {
		description := t.Description
		if t.Name == "heartbeat" {
			description = `Renew a lease explicitly. Leases claimed in this session are renewed
automatically, so this is only for a lease claimed elsewhere, or a
different TTL. A renewal that fails means the lease is gone: claim again
rather than continuing.`
		}
		if t.Name == "claim" {
			description = strings.ReplaceAll(description, "Native HTTP callers heartbeat before expiry and explicitly release or complete\nwith lease_id; response end or disconnect does not renew or release. skift mcp stdio renews\nits claims in the background and attempts release on EOF or handled signal;\na hard kill leaves expiry. Holder writes may renew live leases.", "The stdio process renews its claims and attempts release on EOF or handled\nsignal; a hard kill leaves expiry.")
		}
		out = append(out, &mcpTool{name: t.Name, description: description, schema: t.InputSchema,
			invoke: func(ctx context.Context, args json.RawMessage) mcptools.Result {
				if err != nil {
					return mcptools.Result{IsError: true, Content: []mcptools.Content{{Type: "text", Text: toolErrorText(t.Name, err)}}}
				}
				return t.Invoke(ctx, args)
			},
		})
	}
	return out
}

type stdioLeaseHooks struct{ server *mcpServer }

func (h stdioLeaseHooks) Hold(l *skiftv1.Lease, n *skiftv1.Node) { h.server.hold(l, n) }
func (h stdioLeaseHooks) Forget(id string)                       { h.server.forget(id) }
func (h stdioLeaseHooks) HeldLeases() []mcptools.HeldLease {
	held := h.server.heldLeases()
	out := make([]mcptools.HeldLease, 0, len(held))
	for _, l := range held {
		out = append(out, mcptools.HeldLease{ID: l.id, Ref: l.ref})
	}
	return out
}
