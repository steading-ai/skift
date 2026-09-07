package cli

import (
	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/internal/client"
)

func (a *app) projectCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Create, list, show, and archive projects",
	}
	cmd.AddCommand(a.projectCreate(), a.projectList(), a.projectShow(), a.projectArchive())
	return cmd
}

func (a *app) projectCreate() *cobra.Command {
	var slug, priority, requestID string
	cmd := &cobra.Command{
		Use:   "create <title>",
		Short: "Create a project; the slug is derived from the title unless given",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prio, err := enumFlag[daggerv1.Priority]("priority", priority)
			if err != nil {
				return err
			}
			c, err := a.connect()
			if err != nil {
				return err
			}
			return send(a, cmd, func() (*daggerv1.CreateProjectResponse, error) {
				res, err := c.Graph.CreateProject(cmd.Context(), connect.NewRequest(&daggerv1.CreateProjectRequest{
					RequestId: orMint(requestID), Title: args[0], Slug: slug, Priority: prio,
				}))
				return unwrap(res, err)
			})
		},
	}
	cmd.Flags().StringVar(&slug, "slug", "", "requested slug (default: derived from the title)")
	cmd.Flags().StringVar(&priority, "priority", "", "advisory priority: p0 to p4")
	requestIDFlag(cmd, &requestID)
	return cmd
}

func (a *app) projectList() *cobra.Command {
	var includeArchived bool
	var page pageFlags
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the caller's projects, most recently active first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.connect()
			if err != nil {
				return err
			}
			return send(a, cmd, func() (*daggerv1.ListProjectsResponse, error) {
				res, err := c.Graph.ListProjects(cmd.Context(), connect.NewRequest(&daggerv1.ListProjectsRequest{
					IncludeArchived: includeArchived, PageSize: page.size, PageToken: page.token,
				}))
				return unwrap(res, err)
			})
		},
	}
	cmd.Flags().BoolVar(&includeArchived, "include-archived", false, "include archived projects")
	page.bind(cmd)
	return cmd
}

func (a *app) projectShow() *cobra.Command {
	return &cobra.Command{
		Use:   "show [project]",
		Short: "Show one project by slug, org/slug, or id (default: the current project)",
		Args:  rangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, err := a.projectArg(args)
			if err != nil {
				return err
			}
			c, err := a.connect()
			if err != nil {
				return err
			}
			return send(a, cmd, func() (*daggerv1.GetProjectResponse, error) {
				res, err := c.Graph.GetProject(cmd.Context(), connect.NewRequest(&daggerv1.GetProjectRequest{Project: project}))
				return unwrap(res, err)
			})
		},
	}
}

func (a *app) projectArchive() *cobra.Command {
	var requestID string
	cmd := &cobra.Command{
		Use:   "archive [project]",
		Short: "Make a project read-only (default: the current project)",
		Args:  rangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, err := a.projectArg(args)
			if err != nil {
				return err
			}
			c, err := a.connect()
			if err != nil {
				return err
			}
			return send(a, cmd, func() (*daggerv1.ArchiveProjectResponse, error) {
				res, err := c.Graph.ArchiveProject(cmd.Context(), connect.NewRequest(&daggerv1.ArchiveProjectRequest{
					RequestId: orMint(requestID), Project: project,
				}))
				return unwrap(res, err)
			})
		},
	}
	requestIDFlag(cmd, &requestID)
	return cmd
}

// projectArg is the optional positional project, falling back to the
// current one.
func (a *app) projectArg(args []string) (string, error) {
	if len(args) == 1 {
		return args[0], nil
	}
	return a.project()
}

// Shared flag and argument helpers.

// pageFlags are the paging flags every list command carries.
type pageFlags struct {
	size  int32
	token string
}

func (p *pageFlags) bind(cmd *cobra.Command) {
	cmd.Flags().Int32Var(&p.size, "page-size", 0, "maximum items to return (default: the server's)")
	cmd.Flags().StringVar(&p.token, "page-token", "", "next_page_token of the previous page")
}

// requestIDFlag lets a caller supply the idempotency key of a write, so a
// retried command is the same request rather than a second one.
func requestIDFlag(cmd *cobra.Command, dst *string) {
	cmd.Flags().StringVar(dst, "request-id", "", "idempotency key for this write (default: a fresh ULID)")
}

// orMint returns id, or a fresh request id when it is empty.
func orMint(id string) string {
	if id == "" {
		return client.NewRequestID()
	}
	return id
}

// unwrap turns a Connect response into its message.
func unwrap[T any](res *connect.Response[T], err error) (*T, error) {
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// exactArgs and rangeArgs are cobra's validators reported as usage errors.
func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := cobra.ExactArgs(n)(cmd, args); err != nil {
			return &usageError{err: err}
		}
		return nil
	}
}

func rangeArgs(lo, hi int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := cobra.RangeArgs(lo, hi)(cmd, args); err != nil {
			return &usageError{err: err}
		}
		return nil
	}
}
