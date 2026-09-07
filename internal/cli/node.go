package cli

import (
	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/internal/client"
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
	view := viewFlag{def: daggerv1.View_VIEW_FULL}
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
			return send(a, cmd, func() (*daggerv1.GetNodeResponse, error) {
				res, err := c.Graph.GetNode(cmd.Context(), connect.NewRequest(&daggerv1.GetNodeRequest{Project: project, Node: ref, View: v}))
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
	var page pageFlags
	view := viewFlag{def: daggerv1.View_VIEW_SUMMARY}
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the nodes of the current project, filtered",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			project, err := a.project()
			if err != nil {
				return err
			}
			kind, err := f.assigneeKind()
			if err != nil {
				return err
			}
			st, err := enumFlag[daggerv1.Status]("status", status, "STATUS_", daggerv1.Status_value)
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
			return send(a, cmd, func() (*daggerv1.ListNodesResponse, error) {
				res, err := c.Graph.ListNodes(cmd.Context(), connect.NewRequest(&daggerv1.ListNodesRequest{
					Project: project, Status: st, Kind: f.kind, AssigneeKind: kind, Labels: f.labels, Parent: parent,
					PageSize: page.size, PageToken: page.token, View: v,
				}))
				return unwrap(res, err)
			})
		},
	}
	f.bind(cmd)
	cmd.Flags().StringVar(&status, "status", "", "only nodes in this status: open, done, dropped")
	cmd.Flags().StringVar(&parent, "parent", "", "only children of this node, as a node reference")
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
	cfg, err := a.config()
	if err != nil {
		return "", "", err
	}
	if parsed.ProjectRef() == "" && cfg.Project == "" {
		return "", "", usage("a bare #number reference needs a project; pass --project, set $%s, or qualify the reference as slug#number", client.EnvProject)
	}
	return cfg.Project, parsed.String(), nil
}

// nodeFilters are the filters shared by list, ready, and claim.
type nodeFilters struct {
	kind          string
	assigneeKind_ string
	labels        []string
}

func (f *nodeFilters) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.kind, "kind", "", "only nodes of this kind")
	cmd.Flags().StringVar(&f.assigneeKind_, "assignee-kind", "", "only nodes completable by this kind of principal: agent or user")
	cmd.Flags().StringArrayVarP(&f.labels, "label", "l", nil, "only nodes carrying this label; repeatable")
}

func (f *nodeFilters) assigneeKind() (daggerv1.AssigneeKind, error) {
	return enumFlag[daggerv1.AssigneeKind]("assignee-kind", f.assigneeKind_, "ASSIGNEE_KIND_", daggerv1.AssigneeKind_value)
}
