package server

import (
	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
)

// Views: how much of a node a response carries. Every RPC that returns
// nodes projects them through projectNode on the way out, so "summary"
// means one thing on the wire. The default per RPC is the handler's; see
// the View enum in proto/dagger/v1/service.proto for what each view is
// for.

// viewOr resolves an unspecified view to the RPC's default.
func viewOr(v, def daggerv1.View) daggerv1.View {
	if v == daggerv1.View_VIEW_UNSPECIFIED {
		return def
	}
	return v
}

// projectNode returns n as view shows it. VIEW_FULL is n itself. VIEW_REFS
// is what names the node and says where it stands: number, reference,
// status, and the blocked, ready, and claimed flags. VIEW_SUMMARY adds the
// id, the project (search crosses projects), and what choosing work reads.
// Neither carries the body.
func projectNode(n *daggerv1.Node, view daggerv1.View) *daggerv1.Node {
	if n == nil || view == daggerv1.View_VIEW_FULL {
		return n
	}
	out := &daggerv1.Node{
		Number:    n.GetNumber(),
		Reference: n.GetReference(),
		Status:    n.GetStatus(),
		Blocked:   n.GetBlocked(),
		Ready:     n.GetReady(),
		Claimed:   n.GetClaimed(),
	}
	if view == daggerv1.View_VIEW_SUMMARY {
		out.Id = n.GetId()
		out.ProjectId = n.GetProjectId()
		out.Title = n.GetTitle()
		out.Kind = n.GetKind()
		out.Priority = n.GetPriority()
		out.AssigneeKind = n.GetAssigneeKind()
		out.Assignee = n.GetAssignee()
		out.Labels = n.GetLabels()
		out.HumanTouched = n.GetHumanTouched()
	}
	return out
}
