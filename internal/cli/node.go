package cli

import (
	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/internal/client"
)

func (a *app) nodeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "Inspect nodes",
	}
	cmd.AddCommand(a.nodeShow(), a.nodeList())
	return cmd
}

func (a *app) nodeShow() *cobra.Command {
	view := viewFlag{def: skiftv1.View_VIEW_FULL}
	cmd := &cobra.Command{
		Use:   "show <ref>",
		Short: "Show a node with its edges, result, lease, artifacts, and comments",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, ref, err := a.nodeArg(args[0])
			if err != nil {
				return err
			}
			v, err := view.view()
			if err != nil {
				return err
			}
			c, err := a.connect()
			if err != nil {
				return err
			}
			return send(a, cmd, func() (*skiftv1.GetNodeResponse, error) {
				res, err := c.Graph.GetNode(cmd.Context(), connect.NewRequest(&skiftv1.GetNodeRequest{Project: project, Node: ref, View: v}))
				return unwrap(res, err)
			})
		},
	}
	view.bind(cmd)
	return cmd
}

func (a *app) inputsCommand() *cobra.Command {
	view := viewFlag{def: skiftv1.View_VIEW_FULL}
	cmd := &cobra.Command{
		Use:   "inputs <ref>",
		Short: "Show the results of the predecessors a node depends on",
		Long: `Show what a node depends on: one entry per predecessor that satisfies it,
with the result it completed with and the artifacts it attached. This is
what a downstream agent assembles its context from, and "skift node show"
never returns it: a node's own result is not its predecessors'.

A predecessor still open satisfies nothing and is not an input; "skift
explain" says what a node is waiting on. A dropped one is listed with no
result. Inputs are not transitive.

With --view summary each entry is its reference, title and the size of its
result, so a large handoff can be read one predecessor at a time with "skift
node show".`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, ref, err := a.nodeArg(args[0])
			if err != nil {
				return err
			}
			v, err := view.view()
			if err != nil {
				return err
			}
			c, err := a.connect()
			if err != nil {
				return err
			}
			return send(a, cmd, func() (*skiftv1.GetInputsResponse, error) {
				res, err := c.Graph.GetInputs(cmd.Context(), connect.NewRequest(&skiftv1.GetInputsRequest{Project: project, Node: ref, View: v}))
				return unwrap(res, err)
			})
		},
	}
	view.bind(cmd)
	return cmd
}

func (a *app) nodeList() *cobra.Command {
	var f nodeFilters
	var status, parent string
	var held bool
	var page pageFlags
	view := viewFlag{def: skiftv1.View_VIEW_SUMMARY}
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List nodes, filtered",
		Long: `List nodes matching the filters, by project then number. Scoped to the
current project when one is set, and to every project the caller may read
otherwise.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			projects, err := a.currentProjects()
			if err != nil {
				return err
			}
			kind, err := f.assigneeKind()
			if err != nil {
				return err
			}
			st, err := enumFlag[skiftv1.Status]("status", status)
			if err != nil {
				return err
			}
			v, err := view.view()
			if err != nil {
				return err
			}
			c, err := a.connect()
			if err != nil {
				return err
			}
			filter := &skiftv1.NodeFilter{
				Projects: projects, Status: st, AssigneeKind: kind, Labels: f.labels, Parent: parent, Held: held,
			}
			if f.kind != "" {
				filter.Kinds = []string{f.kind}
			}
			return send(a, cmd, func() (*skiftv1.ListNodesResponse, error) {
				res, err := c.Graph.ListNodes(cmd.Context(), connect.NewRequest(&skiftv1.ListNodesRequest{
					Filter: filter, PageSize: page.size, PageToken: page.token, View: v,
				}))
				return unwrap(res, err)
			})
		},
	}
	f.bind(cmd)
	cmd.Flags().StringVar(&status, "status", "", "only nodes in this status: open, done, dropped")
	cmd.Flags().StringVar(&parent, "parent", "", "only children of this node, as a node reference")
	cmd.Flags().BoolVar(&held, "held", false, "only parked nodes: held themselves, or contained in something held")
	page.bind(cmd)
	view.bind(cmd)
	return cmd
}

// nodeArg parses one reference argument and decides which project the
// request resolves it in. The reference is sent as written; the project
// is only needed for the bare form.
func (a *app) nodeArg(arg string) (project, ref string, err error) {
	parsed, err := parseRef(arg)
	if err != nil {
		return "", "", err
	}
	current, err := a.currentProject()
	if err != nil {
		return "", "", err
	}
	if parsed.ProjectRef() == "" && current == "" {
		return "", "", usage("a bare #number reference needs a project; pass --project, set $%s, or qualify the reference as slug#number", client.EnvProject)
	}
	return current, parsed.String(), nil
}

// nodeFilters are the filters shared by list, ready, and claim.
type nodeFilters struct {
	kind          string
	assigneeKind_ string
	labels        []string
}

func (f *nodeFilters) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.kind, "kind", "", "only nodes of this kind; free-form, conventionally task, bug, spike, or chore")
	cmd.Flags().StringVar(&f.assigneeKind_, "assignee-kind", "", "only nodes completable by this kind of principal: agent or user; a user node is a gate")
	cmd.Flags().StringArrayVarP(&f.labels, "label", "l", nil, "only nodes carrying this label; repeatable")
}

func (f *nodeFilters) assigneeKind() (skiftv1.AssigneeKind, error) {
	return enumFlag[skiftv1.AssigneeKind]("assignee-kind", f.assigneeKind_)
}
