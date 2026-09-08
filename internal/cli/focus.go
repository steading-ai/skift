package cli

import (
	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
)

func (a *app) focusCommand() *cobra.Command {
	var reason, group, requestID string
	var clear bool
	cmd := &cobra.Command{
		Use:   "focus [ref...]",
		Short: "Park everything outside the given projects or nodes",
		Long: `Focus on some work by parking the rest. With no --project the keep-set
names projects and the sweep covers the organization; with one it names
nodes of that project and the sweep covers only that project. The two
compose: an organization-wide focus says which projects are live, and a
project-scoped one narrows it to part of the graph.

Focus replaces rather than stacks. Calling it again in a scope releases
the sweep already there in the same transaction, so --clear never has to
be told which sweep it means.

A sweep parks only what is not already parked, and --clear releases only
what the sweep parked. A hold placed on its own outlives both.

Keeping a node keeps its whole lineage live: what it contains, and what
contains it, since parking a parent would park the node.`,
		Args: usageArgs(cobra.ArbitraryArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, err := a.currentProject()
			if err != nil {
				return err
			}
			if reason == "" {
				return usage("--reason is required: the event log keeps why everything else was parked")
			}
			c, err := a.connect()
			if err != nil {
				return err
			}
			if !clear && group != "" {
				return usage("--group names the sweep to release, so it goes with --clear")
			}
			if clear {
				if len(args) > 0 {
					return usage("--clear takes no keep-set: it releases the sweep, it does not narrow one")
				}
				return send(a, cmd, func() (*daggerv1.ClearFocusResponse, error) {
					res, err := c.Graph.ClearFocus(cmd.Context(), connect.NewRequest(&daggerv1.ClearFocusRequest{
						RequestId: orMint(requestID), Project: project, Group: group, Reason: reason,
					}))
					return unwrap(res, err)
				})
			}
			if len(args) == 0 {
				return usage("name what stays live, or pass --clear to release the sweep")
			}
			return send(a, cmd, func() (*daggerv1.FocusResponse, error) {
				res, err := c.Graph.Focus(cmd.Context(), connect.NewRequest(&daggerv1.FocusRequest{
					RequestId: orMint(requestID), Project: project, Keep: args, Reason: reason,
				}))
				return unwrap(res, err)
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&reason, "reason", "", "why everything else is parked, or why it is coming back; required")
	f.BoolVar(&clear, "clear", false, "release the sweep active in this scope")
	f.StringVar(&group, "group", "", "the sweep to clear; only needed to disambiguate")
	requestIDFlag(cmd, &requestID)
	return cmd
}
