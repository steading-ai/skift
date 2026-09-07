package cli

import (
	"fmt"
	"io"
	"os"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/internal/codec"
)

func (a *app) patchCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "patch",
		Short: "Apply patches: the one way to mutate the graph",
	}
	cmd.AddCommand(a.patchApply())
	return cmd
}

func (a *app) patchApply() *cobra.Command {
	var lease, requestID string
	view := viewFlag{def: daggerv1.View_VIEW_REFS}
	cmd := &cobra.Command{
		Use:   "apply <file|->",
		Short: "Apply a JSON patch from a file, or from stdin with -",
		Long: `Apply a patch: a list of operations applied in one transaction, cycle-checked
as a whole. The input is a dagger.v1.ApplyPatchRequest in JSON. Only "ops" is
required; "project" defaults to the current project and "request_id" to a
fresh ULID. Operations refer to nodes by reference, or by the alias a
create_node operation earlier in the same patch declared:

  {"ops": [
    {"create_node": {"alias": "spike", "title": "Spike the parser"}},
    {"create_node": {"alias": "impl", "title": "Implement it", "blocked_by": ["spike"]}},
    {"add_edge": {"from": "#3", "to": "impl"}}
  ]}

The response lists every node the patch created or changed, with their
numbers and references, and maps each alias to the node it became. It never
echoes the bodies the patch sent; --view full asks for everything, events
included.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req, err := readPatch(cmd, args[0])
			if err != nil {
				return err
			}
			if req.GetProject() == "" {
				project, err := a.project()
				if err != nil {
					return err
				}
				req.Project = project
			}
			if requestID != "" {
				req.RequestId = requestID
			}
			req.RequestId = orMint(req.GetRequestId())
			if lease != "" {
				req.LeaseId = lease
			}
			if req.GetView() == daggerv1.View_VIEW_UNSPECIFIED || cmd.Flags().Changed("view") {
				if req.View, err = view.view(); err != nil {
					return err
				}
			}
			return a.apply(cmd, req)
		},
	}
	cmd.Flags().StringVar(&lease, "lease", "", "fencing token of the lease the caller holds, when the patch touches a leased node")
	requestIDFlag(cmd, &requestID)
	view.bind(cmd)
	return cmd
}

// readPatch reads and parses the patch at path, or stdin for "-".
func readPatch(cmd *cobra.Command, path string) (*daggerv1.ApplyPatchRequest, error) {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(cmd.InOrStdin())
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("read patch: %w", err)
	}
	req := &daggerv1.ApplyPatchRequest{}
	if err := codec.Unmarshal(raw, req); err != nil {
		return nil, decodeUsage(err)
	}
	return req, nil
}

// apply sends one patch and prints the response. Every command that
// mutates the graph ends here.
func (a *app) apply(cmd *cobra.Command, req *daggerv1.ApplyPatchRequest) error {
	c, err := a.connect()
	if err != nil {
		return err
	}
	return send(a, cmd, func() (*daggerv1.ApplyPatchResponse, error) {
		res, err := c.Graph.ApplyPatch(cmd.Context(), connect.NewRequest(req))
		return unwrap(res, err)
	})
}
