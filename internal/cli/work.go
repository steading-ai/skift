package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/durationpb"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/internal/client"
)

// waitForever is what a bare --wait means: block until work is claimed or
// the command is interrupted.
const waitForever = "forever"

// claimChunk bounds each ClaimNext call in a long wait. The server clamps a
// wait to its own ceiling and returns empty when it elapses, so a wait
// longer than that is a loop of bounded calls; this keeps each one well
// inside the ceiling.
const claimChunk = time.Minute

// claimFlags are the flags a claim carries beyond its filters.
type claimFlags struct {
	nodeFilters
	minPriority string
	wait        string
	ttl         time.Duration
	harness     string
	agentRef    string
	sessionRef  string
	requestID   string
}

func (f *claimFlags) bindWait(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.wait, "wait", "", "block until matching work exists, claiming it: a duration, or forever when given bare")
	cmd.Flags().Lookup("wait").NoOptDefVal = waitForever
}

func (f *claimFlags) bindClaim(cmd *cobra.Command) {
	hostname, _ := os.Hostname()
	fl := cmd.Flags()
	fl.DurationVar(&f.ttl, "ttl", 0, "requested lease TTL (default: the server's)")
	fl.StringVar(&f.harness, "harness", "dg", "name of the executing harness, recorded on the lease")
	fl.StringVar(&f.agentRef, "agent-ref", hostname, "where the attempt runs, recorded for placement affinity")
	fl.StringVar(&f.sessionRef, "session-ref", "", "harness session identifier, for a session that works several nodes")
	requestIDFlag(cmd, &f.requestID)
}

func (f *claimFlags) bindFilters(cmd *cobra.Command) {
	f.bind(cmd)
	cmd.Flags().StringVar(&f.minPriority, "min-priority", "", "only nodes at this priority or more urgent: p0 to p4; advisory ordering, so this filters rather than gates")
}

// waitDuration parses --wait. ok is false when the flag was not given.
func (f *claimFlags) waitDuration() (d time.Duration, ok bool, err error) {
	switch f.wait {
	case "":
		return 0, false, nil
	case waitForever:
		return -1, true, nil
	}
	d, err = time.ParseDuration(f.wait)
	if err != nil || d < 0 {
		return 0, false, usage("--wait: %q is not a duration such as 30s or 5m", f.wait)
	}
	return d, true, nil
}

func (a *app) readyCommand() *cobra.Command {
	var f claimFlags
	var page pageFlags
	view := viewFlag{def: daggerv1.View_VIEW_SUMMARY}
	cmd := &cobra.Command{
		Use:   "ready",
		Short: "List the ready set of the current project, or with --wait, block until a node can be claimed",
		Long: `List what is ready: open, unblocked, unleased, with no open children, most
urgent and then oldest first. Nothing is claimed.

With --wait, the command instead blocks until a matching node exists and
returns it claimed under a lease, exactly as "dg claim --wait" does. That is
how a shell loop drives work: the lease id in the response is the fencing
token every later write to the node carries.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			wait, waiting, err := f.waitDuration()
			if err != nil {
				return err
			}
			if waiting {
				return a.claim(cmd, &f, wait)
			}
			project, err := a.project()
			if err != nil {
				return err
			}
			kind, err := f.assigneeKind()
			if err != nil {
				return err
			}
			prio, err := enumFlag[daggerv1.Priority]("min-priority", f.minPriority)
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
			return send(a, cmd, func() (*daggerv1.ReadyResponse, error) {
				res, err := c.Work.Ready(cmd.Context(), connect.NewRequest(&daggerv1.ReadyRequest{
					Project: project, Kind: f.kind, AssigneeKind: kind, Labels: f.labels, MinPriority: prio,
					PageSize: page.size, PageToken: page.token, View: v,
				}))
				return unwrap(res, err)
			})
		},
	}
	f.bindFilters(cmd)
	f.bindWait(cmd)
	f.bindClaim(cmd)
	page.bind(cmd)
	view.bind(cmd)
	return cmd
}

func (a *app) claimCommand() *cobra.Command {
	var f claimFlags
	cmd := &cobra.Command{
		Use:   "claim",
		Short: "Claim the next ready node of the current project under a lease",
		Long: `Claim the most urgent ready node matching the filters. The response carries
the node and its lease; the lease id is the fencing token for every later
write. Without --wait the command returns at once, empty when nothing
matches; with --wait it blocks until something does.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			wait, _, err := f.waitDuration()
			if err != nil {
				return err
			}
			return a.claim(cmd, &f, wait)
		},
	}
	f.bindFilters(cmd)
	f.bindWait(cmd)
	f.bindClaim(cmd)
	return cmd
}

// claim runs ClaimNext until it returns a node or wait elapses. A negative
// wait never elapses. Each round trip is bounded so the server's own wait
// ceiling never cuts a long wait short.
func (a *app) claim(cmd *cobra.Command, f *claimFlags, wait time.Duration) error {
	project, err := a.project()
	if err != nil {
		return err
	}
	kind, err := f.assigneeKind()
	if err != nil {
		return err
	}
	prio, err := enumFlag[daggerv1.Priority]("min-priority", f.minPriority)
	if err != nil {
		return err
	}
	c, err := a.connect()
	if err != nil {
		return err
	}
	req := &daggerv1.ClaimNextRequest{
		RequestId: orMint(f.requestID), Project: project,
		Kind: f.kind, AssigneeKind: kind, Labels: f.labels, MinPriority: prio,
		Harness: f.harness, AgentRef: f.agentRef, SessionRef: f.sessionRef,
	}
	if f.ttl > 0 {
		req.Ttl = durationpb.New(f.ttl)
	}
	return send(a, cmd, func() (*daggerv1.ClaimNextResponse, error) {
		return claimNext(cmd.Context(), c, req, wait)
	})
}

// claimNext is the loop behind claim. The request id is kept across
// iterations: an empty answer writes nothing, so the key is unused until a
// claim lands, and a retry after a dropped connection then returns the
// same node rather than a second one.
func claimNext(ctx context.Context, c *client.Client, req *daggerv1.ClaimNextRequest, wait time.Duration) (*daggerv1.ClaimNextResponse, error) {
	var deadline time.Time
	if wait >= 0 {
		deadline = time.Now().Add(wait)
	}
	for {
		chunk := claimChunk
		if wait >= 0 {
			chunk = min(chunk, time.Until(deadline))
		}
		req.Wait = durationpb.New(max(chunk, 0))
		res, err := c.Work.ClaimNext(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		if res.Msg.GetNode() != nil || wait == 0 || (wait > 0 && !time.Now().Before(deadline)) {
			return res.Msg, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("wait for work: %w", err)
		}
	}
}

func (a *app) heartbeatCommand() *cobra.Command {
	var ttl time.Duration
	var requestID string
	cmd := &cobra.Command{
		Use:   "heartbeat <lease-id>",
		Short: "Renew a live lease",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.connect()
			if err != nil {
				return err
			}
			req := &daggerv1.HeartbeatRequest{RequestId: orMint(requestID), LeaseId: args[0]}
			if ttl > 0 {
				req.Ttl = durationpb.New(ttl)
			}
			return send(a, cmd, func() (*daggerv1.HeartbeatResponse, error) {
				res, err := c.Work.Heartbeat(cmd.Context(), connect.NewRequest(req))
				return unwrap(res, err)
			})
		},
	}
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "requested TTL from now (default: the server's)")
	requestIDFlag(cmd, &requestID)
	return cmd
}

func (a *app) releaseCommand() *cobra.Command {
	var reason, detail, requestID string
	cmd := &cobra.Command{
		Use:   "release <lease-id>",
		Short: "End a lease with a reason, returning the node to the ready set",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := enumFlag[daggerv1.ReleaseReason]("reason", reason)
			if err != nil {
				return err
			}
			if r == daggerv1.ReleaseReason_RELEASE_REASON_UNSPECIFIED {
				return usage("--reason is required: gate, crash, budget, expired, or preempted")
			}
			c, err := a.connect()
			if err != nil {
				return err
			}
			return send(a, cmd, func() (*daggerv1.ReleaseResponse, error) {
				res, err := c.Work.Release(cmd.Context(), connect.NewRequest(&daggerv1.ReleaseRequest{
					RequestId: orMint(requestID), LeaseId: args[0], Reason: r, Detail: detail,
				}))
				return unwrap(res, err)
			})
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why the attempt ended: gate, crash, budget, expired, or preempted")
	cmd.Flags().StringVar(&detail, "detail", "", "free text expanding on the reason")
	requestIDFlag(cmd, &requestID)
	return cmd
}
