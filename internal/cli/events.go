package cli

import (
	"context"
	"errors"
	"os/signal"
	"syscall"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
)

func (a *app) eventsCommand() *cobra.Command {
	var cursor string
	var ops []string
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Stream the change stream, one event per line, until interrupted",
		Long: `Stream events as they are committed, scoped to the current project when one
is set and to the whole organization otherwise. Each line is one JSON
object holding the event and the cursor to resume after it; pass the last
cursor back with --cursor to continue where a previous stream stopped.
Delivery is at least once: consumers deduplicate by event id.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			project, err := a.currentProject()
			if err != nil {
				return err
			}
			p, err := a.printer(cmd)
			if err != nil {
				return err
			}
			// Events are a stream, so text mode is JSON lines too: one
			// object per line is what a pipeline needs.
			p.format = OutputJSON
			c, err := a.connect()
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			stream, err := c.Events.WatchEvents(ctx, connect.NewRequest(&daggerv1.WatchEventsRequest{
				Cursor: cursor, Project: project, Ops: ops,
			}))
			if err != nil {
				return err
			}
			defer stream.Close() //nolint:errcheck // the stream ends with the command
			for stream.Receive() {
				if err := p.emit(stream.Msg()); err != nil {
					return err
				}
			}
			err = stream.Err()
			if err == nil || errors.Is(ctx.Err(), context.Canceled) && connect.CodeOf(err) == connect.CodeCanceled {
				// Interrupted by the user: the stream ended as asked.
				return nil
			}
			return err
		},
	}
	cmd.Flags().StringVar(&cursor, "cursor", "", "resume after this cursor (default: from now)")
	cmd.Flags().StringArrayVar(&ops, "op", nil, "only events of this operation, such as create_node; repeatable")
	return cmd
}
