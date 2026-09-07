package cli

import (
	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
)

// What work costs, from the two ends of it. An agent estimates: a patch
// operation, needing no lease, that says what a node is expected to cost
// and why. An orchestrator reports: an RPC keyed by the lease of the
// attempt whose tokens it counted. The service refuses a report from
// anyone else, so there is no report verb an agent can usefully run.

// tokenFlags are the four counts every usage record carries. Counts,
// never currency: a rate table is applied by whoever wants money.
type tokenFlags struct {
	input      int64
	output     int64
	cacheRead  int64
	cacheWrite int64
	model      string
}

func (t *tokenFlags) bind(cmd *cobra.Command, expected bool) {
	tense := "spent"
	if expected {
		tense = "expected"
	}
	f := cmd.Flags()
	f.Int64Var(&t.input, "input", 0, "input tokens "+tense)
	f.Int64Var(&t.output, "output", 0, "output tokens "+tense)
	f.Int64Var(&t.cacheRead, "cache-read", 0, "tokens "+tense+" from the model's prompt cache")
	f.Int64Var(&t.cacheWrite, "cache-write", 0, "tokens "+tense+" written to the model's prompt cache")
	f.StringVar(&t.model, "model", "", "name of the model, free-form; the rollup groups by it")
}

func (a *app) estimateCommand() *cobra.Command {
	var tokens tokenFlags
	var basis, requestID string
	view := viewFlag{def: daggerv1.View_VIEW_REFS}
	cmd := &cobra.Command{
		Use:   "estimate <ref>",
		Short: "Record what a node is expected to cost, and why",
		Long: `Record an estimate: what a node is expected to cost in tokens, and the
basis it was reasoned from. An estimate needs no lease and changes neither
the node nor its readiness, so an agent planning work estimates the nodes
it files without claiming them.

Estimating again adds a record rather than replacing one; the node's
current estimate is the most recent, and "dg node show" carries it beside
the rollup of what the node and its descendants have actually cost.

Reporting what an attempt did cost is the orchestrator's, through
"dg usage report".`,
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
			v, err := view.view()
			if err != nil {
				return err
			}
			return a.apply(cmd, &daggerv1.ApplyPatchRequest{
				RequestId: orMint(requestID),
				Project:   project,
				Ops: []*daggerv1.PatchOp{{Op: &daggerv1.PatchOp_EstimateNode{EstimateNode: &daggerv1.EstimateNodeOp{
					Node: ref.String(), Model: tokens.model, Basis: basis,
					InputTokens: tokens.input, OutputTokens: tokens.output,
					CacheReadTokens: tokens.cacheRead, CacheWriteTokens: tokens.cacheWrite,
				}}}},
				View: v,
			})
		},
	}
	tokens.bind(cmd, true)
	cmd.Flags().StringVar(&basis, "basis", "", "what the estimate was reasoned from; required, so the number can be revisited")
	requestIDFlag(cmd, &requestID)
	view.bind(cmd)
	return cmd
}

func (a *app) usageCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Report what an attempt cost",
	}
	cmd.AddCommand(a.usageReport())
	return cmd
}

func (a *app) usageReport() *cobra.Command {
	var tokens tokenFlags
	var lease, requestID string
	cmd := &cobra.Command{
		Use:   "report <ref>",
		Short: "Report what an attempt on a node has cost, under its lease",
		Long: `Report an actual: the tokens one attempt on a node has spent so far,
attributed to the lease that attempt runs under. Reporting again adds a
record rather than replacing one, so the attempt's cost is their sum, and
a retry under a new lease is counted apart from the first.

This is the orchestrator's call, and the service refuses it from an agent
principal: the process running an agent is the one that can count its
tokens. An agent records an estimate with "dg estimate" instead.`,
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
			if lease == "" {
				return usage("--lease is required: an actual is attributed to the attempt that spent it")
			}
			c, err := a.connect()
			if err != nil {
				return err
			}
			return send(a, cmd, func() (*daggerv1.ReportUsageResponse, error) {
				res, err := c.Work.ReportUsage(cmd.Context(), connect.NewRequest(&daggerv1.ReportUsageRequest{
					RequestId: orMint(requestID), Project: project, LeaseId: lease, Node: ref.String(),
					Model: tokens.model, InputTokens: tokens.input, OutputTokens: tokens.output,
					CacheReadTokens: tokens.cacheRead, CacheWriteTokens: tokens.cacheWrite,
				}))
				return unwrap(res, err)
			})
		},
	}
	tokens.bind(cmd, false)
	cmd.Flags().StringVar(&lease, "lease", "", "fencing token of the lease the attempt holds on the node")
	requestIDFlag(cmd, &requestID)
	return cmd
}
