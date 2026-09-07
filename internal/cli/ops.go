package cli

import (
	"slices"
	"strings"

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
			if op.Priority, err = enumFlag[daggerv1.Priority]("priority", priority); err != nil {
				return err
			}
			if op.AssigneeKind, err = enumFlag[daggerv1.AssigneeKind]("assignee-kind", assigneeKind); err != nil {
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

// updateNodeFieldFlags are the flags that set a field of UpdateNodeOp. One
// of them must be given: an update that changes nothing is a mistake, not
// an empty patch.
var updateNodeFieldFlags = []string{"title", "body", "kind", "priority", "parent", "label", "assignee-kind", "assignee"}

func (a *app) updateCommand() *cobra.Command {
	var title, body, kind, priority, parent, assignee, assigneeKind, lease, requestID string
	var labels []string
	view := viewFlag{def: daggerv1.View_VIEW_REFS}
	cmd := &cobra.Command{
		Use:   "update <ref>",
		Short: "Change the mutable fields of a node: title, body, kind, priority, parent, labels, assignee",
		Long: `Change a node in place. Only the flags you pass are sent, so everything
else is left alone; status is not here because it changes through complete,
drop and reopen alone.

An empty value is a value: --parent '' makes the node a root, --assignee ''
unassigns, and --label '' clears the label set. --label otherwise replaces
the whole set rather than adding to it.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := parseRef(args[0])
			if err != nil {
				return err
			}
			f := cmd.Flags()
			if !slices.ContainsFunc(updateNodeFieldFlags, f.Changed) {
				return usage("dg update needs a field to change: --%s", strings.Join(updateNodeFieldFlags, ", --"))
			}
			op := &daggerv1.UpdateNodeOp{Node: ref.String()}
			refs := []client.NodeRef{ref}
			if f.Changed("title") {
				op.Title = &title
			}
			if f.Changed("body") {
				b, err := readBody(cmd, body)
				if err != nil {
					return err
				}
				op.Body = &b
			}
			if f.Changed("kind") {
				op.Kind = &kind
			}
			if f.Changed("priority") {
				p, err := requiredEnumFlag[daggerv1.Priority]("priority", priority)
				if err != nil {
					return err
				}
				op.Priority = &p
			}
			if f.Changed("parent") {
				if parent != "" {
					parentRef, err := parseRef(parent)
					if err != nil {
						return err
					}
					refs = append(refs, parentRef)
				}
				op.Parent = &parent
			}
			if f.Changed("label") {
				op.Labels = &daggerv1.Labels{Values: nonEmpty(labels)}
			}
			if f.Changed("assignee-kind") {
				k, err := requiredEnumFlag[daggerv1.AssigneeKind]("assignee-kind", assigneeKind)
				if err != nil {
					return err
				}
				op.AssigneeKind = &k
			}
			if f.Changed("assignee") {
				op.Assignee = &assignee
			}
			project, err := a.projectFor(refs...)
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
				Ops:       []*daggerv1.PatchOp{{Op: &daggerv1.PatchOp_UpdateNode{UpdateNode: op}}},
				View:      v,
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&title, "title", "", "new title")
	f.StringVar(&body, "body", "", "new markdown body, the prompt an agent receives; - reads stdin")
	f.StringVar(&kind, "kind", "", "new single-valued label such as task, bug, spike, or chore")
	f.StringVar(&priority, "priority", "", "new advisory priority: p0 to p4")
	f.StringVar(&parent, "parent", "", "node reference of the new parent; the empty string makes the node a root")
	f.StringArrayVarP(&labels, "label", "l", nil, "label; repeatable, and replaces the whole set")
	f.StringVar(&assigneeKind, "assignee-kind", "", "who may complete the node: agent or user; user makes it a gate")
	f.StringVar(&assignee, "assignee", "", "id of a specific principal to assign to; the empty string unassigns")
	f.StringVar(&lease, "lease", "", "fencing token of the lease held on the node, if any")
	requestIDFlag(cmd, &requestID)
	view.bind(cmd)
	return cmd
}

// nonEmpty drops empty values from a repeatable flag, so that an empty
// --label is a label set with nothing in it rather than one empty label.
func nonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
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
			edgeKind, err := enumFlag[daggerv1.EdgeKind]("kind", kind)
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

func (a *app) dropCommand() *cobra.Command {
	var reason, detail, requestID string
	var maxNodes int
	view := viewFlag{def: daggerv1.View_VIEW_REFS}
	cmd := &cobra.Command{
		Use:   "drop <ref>",
		Short: "Drop a node with a reason, optionally cascading within a stated bound",
		Long: `Drop a node. Without --cascade the drop asserts that downstream work does
not need this node after all, and what it blocks becomes ready. With
--cascade N it asserts the functionality is gone and takes the whole
subtree the node blocks, refusing the patch if that subtree is larger than
N nodes and naming the real count; N counts the node named here. Read the
graph first: the bound is a statement that you have.

The response lists the reference of every node dropped, which is what a
reopen undoing the drop works from.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := parseRef(args[0])
			if err != nil {
				return err
			}
			dropReason, err := enumFlag[daggerv1.DropReason]("reason", reason)
			if err != nil {
				return err
			}
			if dropReason == daggerv1.DropReason_DROP_REASON_UNSPECIFIED {
				return usage("--reason is required: a drop is an assertion about the work, and the event keeps it")
			}
			op := &daggerv1.DropNodeOp{Node: ref.String(), Reason: dropReason, Detail: detail}
			if cmd.Flags().Changed("cascade") {
				if maxNodes < 1 {
					return usage("--cascade takes the most nodes the cascade may drop, at least 1")
				}
				op.Cascade = &daggerv1.Cascade{MaxNodes: int32(maxNodes)} //nolint:gosec // bounded below by the check above and above by the patch ceiling the server applies
			}
			project, err := a.projectFor(ref)
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
				Ops:       []*daggerv1.PatchOp{{Op: &daggerv1.PatchOp_DropNode{DropNode: op}}},
				View:      v,
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&reason, "reason", "", "why: superseded, not_viable, no_longer_relevant, or other")
	f.StringVar(&detail, "detail", "", "free text expanding on the reason; required when the reason is other")
	f.IntVar(&maxNodes, "cascade", 0, "also drop the subtree this node blocks, refusing more than this many nodes in all")
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
