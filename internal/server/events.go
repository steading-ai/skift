package server

import (
	"context"

	"connectrpc.com/connect"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/gen/dagger/v1/daggerv1connect"
	"github.com/ivy/dagger/internal/domain"
)

// EventService is the dagger.v1.EventService handler: the change stream.
// Like the others it translates and decides nothing; the cursor, the
// wake signal, and the policy filter are the domain's.
type EventService struct {
	svc *domain.Service
}

var _ daggerv1connect.EventServiceHandler = (*EventService)(nil)

// NewEventService returns the EventService handler over svc.
func NewEventService(svc *domain.Service) *EventService {
	return &EventService{svc: svc}
}

// WatchEvents implements daggerv1connect.EventServiceHandler. A stream
// ends cleanly when the server starts draining; the consumer resumes
// from its last cursor. A message without an event carries a position
// alone: the stream's opening position, one after an interval the policy
// filtered entirely, or a refresh of a quiet stream.
func (e *EventService) WatchEvents(ctx context.Context, req *connect.Request[daggerv1.WatchEventsRequest], stream *connect.ServerStream[daggerv1.WatchEventsResponse]) error {
	orgID, actor, err := caller(ctx)
	if err != nil {
		return err
	}
	err = e.svc.WatchEvents(ctx, orgID, actor, domain.WatchRequest{
		Cursor:   req.Msg.GetCursor(),
		Project:  req.Msg.GetProject(),
		Ops:      req.Msg.GetOps(),
		PageSize: defaultPageSize,
		Stop:     Draining(ctx),
	}, func(ev domain.WatchEvent) error {
		res := &daggerv1.WatchEventsResponse{Cursor: ev.Cursor}
		if ev.Event != nil {
			var err error
			if res.Event, err = toEvent(*ev.Event); err != nil {
				return err
			}
		}
		return stream.Send(res)
	})
	return connectError(err)
}
