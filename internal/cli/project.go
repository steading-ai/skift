package cli

import (
	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/internal/client"
)

func (a *app) projectCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Create, list, show, update, archive, hold, and release projects",
	}
	cmd.AddCommand(a.projectCreate(), a.projectList(), a.projectShow(), a.projectUpdate(), a.projectArchive(),
		a.projectHold(), a.projectUnhold())
	return cmd
}

func (a *app) projectCreate() *cobra.Command {
	var slug, priority, vcsURL, requestID string
	cmd := &cobra.Command{
		Use:   "create <title>",
		Short: "Create a project; the slug is derived from the title unless given",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prio, err := enumFlag[skiftv1.Priority]("priority", priority)
			if err != nil {
				return err
			}
			c, err := a.connect()
			if err != nil {
				return err
			}
			return send(a, cmd, func() (*skiftv1.CreateProjectResponse, error) {
				res, err := c.Graph.CreateProject(cmd.Context(), connect.NewRequest(&skiftv1.CreateProjectRequest{
					RequestId: orMint(requestID), Title: args[0], Slug: slug, Priority: prio, VcsUrl: vcsURL,
				}))
				return unwrap(res, err)
			})
		},
	}
	cmd.Flags().StringVar(&slug, "slug", "", "requested slug (default: derived from the title)")
	cmd.Flags().StringVar(&priority, "priority", "", "advisory priority: p0 to p4")
	cmd.Flags().StringVar(&vcsURL, "vcs-url", "", "clone URL of the repository the project delivers to")
	requestIDFlag(cmd, &requestID)
	return cmd
}

func (a *app) projectUpdate() *cobra.Command {
	var vcsURL, requestID string
	cmd := &cobra.Command{
		Use:   "update [project]",
		Short: "Change what a project carries about itself (default: the current project)",
		Args:  usageArgs(cobra.RangeArgs(0, 1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, err := a.projectArg(args)
			if err != nil {
				return err
			}
			req := &skiftv1.UpdateProjectRequest{RequestId: orMint(requestID), Project: project}
			// A flag left alone leaves the field alone; `--vcs-url ''` is
			// the way to clear it, which is why presence and not the value
			// decides.
			if cmd.Flags().Changed("vcs-url") {
				req.VcsUrl = &vcsURL
			}
			c, err := a.connect()
			if err != nil {
				return err
			}
			return send(a, cmd, func() (*skiftv1.UpdateProjectResponse, error) {
				res, err := c.Graph.UpdateProject(cmd.Context(), connect.NewRequest(req))
				return unwrap(res, err)
			})
		},
	}
	cmd.Flags().StringVar(&vcsURL, "vcs-url", "", "clone URL of the repository the project delivers to; empty clears it")
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
			return send(a, cmd, func() (*skiftv1.ListProjectsResponse, error) {
				res, err := c.Graph.ListProjects(cmd.Context(), connect.NewRequest(&skiftv1.ListProjectsRequest{
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
		Args:  usageArgs(cobra.RangeArgs(0, 1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, err := a.projectArg(args)
			if err != nil {
				return err
			}
			c, err := a.connect()
			if err != nil {
				return err
			}
			return send(a, cmd, func() (*skiftv1.GetProjectResponse, error) {
				res, err := c.Graph.GetProject(cmd.Context(), connect.NewRequest(&skiftv1.GetProjectRequest{Project: project}))
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
		Args:  usageArgs(cobra.RangeArgs(0, 1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, err := a.projectArg(args)
			if err != nil {
				return err
			}
			c, err := a.connect()
			if err != nil {
				return err
			}
			return send(a, cmd, func() (*skiftv1.ArchiveProjectResponse, error) {
				res, err := c.Graph.ArchiveProject(cmd.Context(), connect.NewRequest(&skiftv1.ArchiveProjectRequest{
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
	return usageArgs(cobra.ExactArgs(n))
}

// usageArgs reports a command's arity error as a usage error, so every
// command's argument checking exits the same way whatever shape it takes.
func usageArgs(v cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := v(cmd, args); err != nil {
			return &usageError{err: err}
		}
		return nil
	}
}

// projectHold and projectUnhold are the same command in two directions:
// the RPC and the wording differ, nothing else does.
func (a *app) projectHold() *cobra.Command {
	cmd := a.projectHoldCommand("hold", "Park a project, with a reason (default: the current project)",
		`Park a project. Every node in it leaves the ready set and no status
changes. Unlike archiving, the project stays writable, so parked work can
still be planned, decomposed, and commented on.

The response says how many open nodes left the ready set.`,
		"why the work is parked; required", reasonRequired,
		func(cmd *cobra.Command, c *client.Client, req *skiftv1.HoldProjectRequest) error {
			return send(a, cmd, func() (*skiftv1.HoldProjectResponse, error) {
				return unwrap(c.Graph.HoldProject(cmd.Context(), connect.NewRequest(req)))
			})
		})
	return cmd
}

func (a *app) projectUnhold() *cobra.Command {
	return a.projectHoldCommand("unhold", "Release a project's hold, with a reason (default: the current project)",
		"", "why the work is coming back; required",
		"--reason is required: the event log keeps why the work came back",
		func(cmd *cobra.Command, c *client.Client, req *skiftv1.HoldProjectRequest) error {
			return send(a, cmd, func() (*skiftv1.UnholdProjectResponse, error) {
				return unwrap(c.Graph.UnholdProject(cmd.Context(), connect.NewRequest(&skiftv1.UnholdProjectRequest{
					RequestId: req.GetRequestId(), Project: req.GetProject(), Reason: req.GetReason(),
				})))
			})
		})
}

// projectHoldCommand builds one project-level hold command over the call
// that distinguishes it.
func (a *app) projectHoldCommand(use, short, long, reasonHelp, missingReason string,
	call func(*cobra.Command, *client.Client, *skiftv1.HoldProjectRequest) error,
) *cobra.Command {
	var reason, requestID string
	cmd := &cobra.Command{
		Use:   use + " [project]",
		Short: short,
		Long:  long,
		Args:  usageArgs(cobra.RangeArgs(0, 1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, err := a.projectArg(args)
			if err != nil {
				return err
			}
			if reason == "" {
				return usage("%s", missingReason)
			}
			c, err := a.connect()
			if err != nil {
				return err
			}
			return call(cmd, c, &skiftv1.HoldProjectRequest{
				RequestId: orMint(requestID), Project: project, Reason: reason,
			})
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", reasonHelp)
	requestIDFlag(cmd, &requestID)
	return cmd
}
