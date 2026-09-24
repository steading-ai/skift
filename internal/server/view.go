package server

import (
	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
)

// Views: how much of a node a response carries. Every RPC that returns
// nodes projects them through projectNode on the way out, so "summary"
// means one thing on the wire. The default per RPC is the handler's; see
// the View enum in proto/skift/v1/service.proto for what each view is
// for.

// viewOr resolves an unspecified view to the RPC's default.
func viewOr(v, def skiftv1.View) skiftv1.View {
	if v == skiftv1.View_VIEW_UNSPECIFIED {
		return def
	}
	return v
}

// projectNode returns n as view shows it. VIEW_FULL is n itself. VIEW_REFS
// is what names the node and says where it stands: number, reference,
// status, and the blocked, ready, claimed, and held flags. VIEW_SUMMARY
// adds the id, the project (search crosses projects), the hold parking the
// node if it carries one, and what choosing work reads. Neither carries the
// body.
func projectNode(n *skiftv1.Node, view skiftv1.View) *skiftv1.Node {
	if n == nil || view == skiftv1.View_VIEW_FULL {
		return n
	}
	out := &skiftv1.Node{
		Number:    n.GetNumber(),
		Reference: n.GetReference(),
		Status:    n.GetStatus(),
		Blocked:   n.GetBlocked(),
		Ready:     n.GetReady(),
		Claimed:   n.GetClaimed(),
		Held:      n.GetHeld(),
	}
	if view == skiftv1.View_VIEW_SUMMARY {
		out.Id = n.GetId()
		out.ProjectId = n.GetProjectId()
		out.Title = n.GetTitle()
		out.Kind = n.GetKind()
		out.Priority = n.GetPriority()
		out.AssigneeKind = n.GetAssigneeKind()
		out.Assignee = n.GetAssignee()
		out.Labels = n.GetLabels()
		out.HumanTouched = n.GetHumanTouched()
		out.Hold = n.GetHold()
	}
	return out
}
