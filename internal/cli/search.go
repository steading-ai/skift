package cli

import (
	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
)

func (a *app) searchCommand() *cobra.Command {
	var includeArchived bool
	var page pageFlags
	view := viewFlag{def: daggerv1.View_VIEW_SUMMARY}
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search titles, bodies, results, and comments across the organization",
		Long: `Search lexically, in PostgreSQL websearch syntax: words, "quoted phrases",
-excluded words, and OR. Every project the caller may read is searched
unless --project narrows it.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, err := a.currentProject()
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
			return send(a, cmd, func() (*daggerv1.SearchResponse, error) {
				res, err := c.Graph.Search(cmd.Context(), connect.NewRequest(&daggerv1.SearchRequest{
					Query: args[0], Project: project, IncludeArchived: includeArchived,
					PageSize: page.size, PageToken: page.token, View: v,
				}))
				return unwrap(res, err)
			})
		},
	}
	cmd.Flags().BoolVar(&includeArchived, "include-archived", false, "include nodes of archived projects")
	page.bind(cmd)
	view.bind(cmd)
	return cmd
}

func (a *app) explainCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "explain <ref>",
		Short: "Say why a node is not ready; no reasons means it is",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, ref, err := a.nodeArg(args[0])
			if err != nil {
				return err
			}
			c, err := a.connect()
			if err != nil {
				return err
			}
			return send(a, cmd, func() (*daggerv1.ExplainResponse, error) {
				res, err := c.Graph.Explain(cmd.Context(), connect.NewRequest(&daggerv1.ExplainRequest{Project: project, Node: ref}))
				return unwrap(res, err)
			})
		},
	}
}
