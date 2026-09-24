package server

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"github.com/steading-ai/skift/internal/domain"
)

// Every error a handler returns is a Connect error whose code says what
// kind of failure it was, so a client can branch on the code and a person
// can read the message. The domain reports failures as typed errors; this
// is the one place they become codes.

// connectError maps err to a Connect error. A Connect error passes through
// unchanged. Anything the domain did not type is internal.
func connectError(err error) error {
	if err == nil {
		return nil
	}
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return err
	}
	return connect.NewError(codeOf(err), err)
}

// codeOf is the Connect code for one domain error. Typed errors are
// matched with errors.As, sentinels with errors.Is, so wrapping never hides
// the code: an *OpError naming the operation that failed still reports
// its cause's code.
func codeOf(err error) connect.Code {
	var (
		limit    *domain.LimitError
		denial   *domain.Denial
		notFound *domain.NotFoundError
		cycle    *domain.CycleError
		conflict *domain.RequestConflictError
		parse    *domain.ParseError
		cascade  *domain.CascadeTooLargeError
	)
	switch {
	case errors.Is(err, context.Canceled):
		return connect.CodeCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return connect.CodeDeadlineExceeded

	// Too much was asked for: a page, a patch, or a body over its ceiling.
	case errors.As(err, &limit):
		return connect.CodeResourceExhausted

	// The policy refused the resolved operation.
	case errors.As(err, &denial):
		return connect.CodePermissionDenied

	// A reference resolved to nothing.
	case errors.As(err, &notFound),
		errors.Is(err, domain.ErrNodeNotFound),
		errors.Is(err, domain.ErrProjectNotFound),
		errors.Is(err, domain.ErrCommentNotFound),
		errors.Is(err, domain.ErrBlobNotFound),
		errors.Is(err, domain.ErrPrincipalNotFound):
		return connect.CodeNotFound

	// The graph is not in the state the request needs: a cycle, a subtree
	// larger than the cascade that would take it stated, a fence the
	// write does not pass, an archived project, a transition from the
	// wrong status. Distinct from a permission denial: nothing about who
	// asked is at issue, only what the graph looks like now.
	case errors.As(err, &cycle),
		errors.As(err, &cascade),
		errors.Is(err, domain.ErrLeaseNotLive),
		errors.Is(err, domain.ErrNotLeaseHolder),
		errors.Is(err, domain.ErrLeaseRequired),
		errors.Is(err, domain.ErrLeaseMismatch),
		errors.Is(err, domain.ErrProjectArchived),
		errors.Is(err, domain.ErrStatusConflict),
		errors.Is(err, domain.ErrCommentResolved),
		errors.Is(err, domain.ErrCommentUnchanged),
		errors.Is(err, domain.ErrReplyAcrossNodes),
		errors.Is(err, domain.ErrArtifactNameTaken),
		errors.Is(err, domain.ErrBlobElsewhere):
		return connect.CodeFailedPrecondition

	// Lost every attempt to a concurrent claimant; the caller retries.
	case errors.Is(err, domain.ErrClaimContended):
		return connect.CodeAborted

	// The request id was already used for a different request.
	case errors.As(err, &conflict):
		return connect.CodeAlreadyExists

	// The cursor points before the replay window; the consumer
	// resynchronizes rather than resuming.
	case errors.Is(err, domain.ErrCursorExpired):
		return connect.CodeOutOfRange

	// The request itself is malformed.
	case errors.As(err, &parse),
		errors.Is(err, domain.ErrCursorInvalid),
		errors.Is(err, domain.ErrNoRequestID),
		errors.Is(err, domain.ErrNoOperations),
		errors.Is(err, domain.ErrReasonRequired),
		errors.Is(err, domain.ErrCascadeBoundRequired),
		errors.Is(err, domain.ErrResultRequired),
		errors.Is(err, domain.ErrNegativeDuration),
		errors.Is(err, domain.ErrNoLeaseID),
		errors.Is(err, domain.ErrSearchQueryRequired),
		errors.Is(err, domain.ErrNoDefaultProject),
		errors.Is(err, domain.ErrOneProject),
		errors.Is(err, domain.ErrProjectRequired),
		errors.Is(err, domain.ErrTitleRequired),
		errors.Is(err, domain.ErrVCSURLInvalid),
		errors.Is(err, domain.ErrCommentBodyRequired),
		errors.Is(err, domain.ErrDigestMismatch),
		errors.Is(err, domain.ErrGitCommitRequired),
		errors.Is(err, domain.ErrNegativeTokens),
		errors.Is(err, domain.ErrNoTokens),
		errors.Is(err, domain.ErrBasisRequired),
		errors.Is(err, domain.ErrUsageNeedsLease):
		return connect.CodeInvalidArgument

	// The transport attributed nothing; the interceptor should have
	// refused the call before it got here.
	case errors.Is(err, domain.ErrNoActor):
		return connect.CodeUnauthenticated
	}
	return connect.CodeInternal
}
