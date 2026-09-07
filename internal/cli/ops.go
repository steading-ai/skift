package cli

import (
	"github.com/spf13/cobra"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/internal/client"
)

// The single-operation commands: each is one patch with one op, for the
// common cases a shell one-liner covers. Anything more is `dg patch apply`.

func (a *app) createCommand() *cobra.Command {
	op := &daggerv1.CreateNodeOp{}
	var body, priority, assigneeKind, requestID string
	view := viewFlag{def: daggerv1.View_VIEW_REFS}
	cmd := &cobra.Command{
		Use:   "create <title>",
		Short: "Create a node in the current project",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var err error
			if op.Priority, err = enumFlag[daggerv1.Priority]("priority", priority, "PRIORITY_", daggerv1.Priority_value); err != nil {
				return err
			}
			if op.AssigneeKind, err = enumFlag[daggerv1.AssigneeKind]("assignee-kind", assigneeKind, "ASSIGNEE_KIND_", daggerv1.AssigneeKind_value); err != nil {
				return err
			}
			refs := make([]client.NodeRef, 0, 1+len(op.GetBlockedBy()))
			if op.GetParent() != "" {
				ref, err := parseRef(op.GetParent())
				if err != nil {
					return err
				}
				refs = append(refs, ref)
			}
			for _, b := range op.GetBlockedBy() {
				ref, err := parseRef(b)
				if err != nil {
					return err
				}
				refs = append(refs, ref)
			}
			project, err := a.projectFor(refs...)
			if err != nil {
				return err
			}
			if op.Body, err = readBody(cmd, body); err != nil {
				return err
			}
			v, err := view.view()
			if err != nil {
				return err
			}
			op.Title = args[0]
			return a.apply(cmd, &daggerv1.ApplyPatchRequest{
				RequestId: orMint(requestID),
				Project:   project,
				Ops:       []*daggerv1.PatchOp{{Op: &daggerv1.PatchOp_CreateNode{CreateNode: op}}},
				View:      v,
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&body, "body", "", "markdown body, the prompt an agent receives; - reads stdin")
	f.StringVar(&op.Kind, "kind", "", "single-valued label such as task, bug, spike, or chore (default: task)")
	f.StringVar(&priority, "priority", "", "advisory priority: p0 to p4 (default: p2)")
	f.StringVar(&op.Parent, "parent", "", "node reference of the parent")
	f.StringArrayVar(&op.BlockedBy, "blocked-by", nil, "node reference that must finish first; repeatable")
	f.StringVar(&assigneeKind, "assignee-kind", "", "who may complete the node: agent or user; user makes it a gate")
	f.StringVar(&op.Assignee, "assignee", "", "id of a specific principal to assign to")
	f.StringArrayVarP(&op.Labels, "label", "l", nil, "label; repeatable")
	requestIDFlag(cmd, &requestID)
	view.bind(cmd)
	return cmd
}

func (a *app) linkCommand() *cobra.Command {
	var kind, requestID string
	view := viewFlag{def: daggerv1.View_VIEW_REFS}
	cmd := &cobra.Command{
		Use:   "link <from> <to>",
		Short: "Add an edge: for blocks, <from> must finish before <to> is ready",
		Args:  exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			from, err := parseRef(args[0])
			if err != nil {
				return err
			}
			to, err := parseRef(args[1])
			if err != nil {
				return err
			}
			edgeKind, err := enumFlag[daggerv1.EdgeKind]("kind", kind, "EDGE_KIND_", daggerv1.EdgeKind_value)
			if err != nil {
				return err
			}
			project, err := a.projectFor(from, to)
			if err != nil {
				return err
			}
			v, err := view.view()
			if err != nil {
				return err
			}
			return a.apply(cmd, &daggerv1.ApplyPatchRequest{
				RequestId: orMint(requestID),
				Project:   project,
				Ops: []*daggerv1.PatchOp{{Op: &daggerv1.PatchOp_AddEdge{AddEdge: &daggerv1.AddEdgeOp{
					From: from.String(), To: to.String(), Kind: edgeKind,
				}}}},
				View: v,
			})
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "edge kind (default: blocks)")
	requestIDFlag(cmd, &requestID)
	view.bind(cmd)
	return cmd
}

func (a *app) completeCommand() *cobra.Command {
	var lease, result, requestID string
	view := viewFlag{def: daggerv1.View_VIEW_REFS}
	cmd := &cobra.Command{
		Use:   "complete <ref>",
		Short: "Complete a node with a result, under the lease that holds it",
		Long: `Complete a node. The result body is what downstream agents assemble their
context from, so it should say what was produced and where. A node under a
live lease can only be completed by the lease holder, carrying the lease id
as the fencing token; the lease is released as part of the same patch.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := parseRef(args[0])
			if err != nil {
				return err
			}
			project, err := a.projectFor(ref)
			if err != nil {
				return err
			}
			body, err := readBody(cmd, result)
			if err != nil {
				return err
			}
			v, err := view.view()
			if err != nil {
				return err
			}
			return a.apply(cmd, &daggerv1.ApplyPatchRequest{
				RequestId: orMint(requestID),
				Project:   project,
				LeaseId:   lease,
				Ops: []*daggerv1.PatchOp{{Op: &daggerv1.PatchOp_CompleteNode{CompleteNode: &daggerv1.CompleteNodeOp{
					Node: ref.String(), Result: &daggerv1.Result{Body: body},
				}}}},
				View: v,
			})
		},
	}
	cmd.Flags().StringVar(&lease, "lease", "", "fencing token of the lease held on the node")
	cmd.Flags().StringVar(&result, "result", "", "result body in markdown; - reads stdin")
	requestIDFlag(cmd, &requestID)
	view.bind(cmd)
	return cmd
}

func (a *app) commentCommand() *cobra.Command {
	var inReplyTo, lease, requestID string
	view := viewFlag{def: daggerv1.View_VIEW_REFS}
	cmd := &cobra.Command{
		Use:   "comment <ref> <body|->",
		Short: "Add a comment to a node",
		Args:  exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := parseRef(args[0])
			if err != nil {
				return err
			}
			project, err := a.projectFor(ref)
			if err != nil {
				return err
			}
			body, err := readBody(cmd, args[1])
			if err != nil {
				return err
			}
			v, err := view.view()
			if err != nil {
				return err
			}
			return a.apply(cmd, &daggerv1.ApplyPatchRequest{
				RequestId: orMint(requestID),
				Project:   project,
				LeaseId:   lease,
				Ops: []*daggerv1.PatchOp{{Op: &daggerv1.PatchOp_AddComment{AddComment: &daggerv1.AddCommentOp{
					Node: ref.String(), Body: body, InReplyTo: inReplyTo,
				}}}},
				View: v,
			})
		},
	}
	cmd.Flags().StringVar(&inReplyTo, "in-reply-to", "", "id of the comment this one replies to")
	cmd.Flags().StringVar(&lease, "lease", "", "fencing token of the lease held on the node, if any")
	requestIDFlag(cmd, &requestID)
	view.bind(cmd)
	return cmd
}
