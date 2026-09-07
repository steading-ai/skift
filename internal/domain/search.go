package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ivy/dagger/internal/policy"
	"github.com/ivy/dagger/internal/store"
)

// Lexical search over node titles and bodies, results, and comments,
// scoped to one organization and filtered by policy. The matching is
// PostgreSQL full text over the GIN-indexed documents migration
// 00012_search.sql defines; see the Search section of docs/domain-model.md.
//
// Search is a read, so it writes no event and audits no denial. It is
// still authorized: every hit is put to the policy as a `search` of that
// node, and a hit the policy refuses is left out of the page. The default
// bundle lets every principal of an organization search all of it; a
// deployment that narrows reads does so in policy, where the rows are.

// Errors a search is rejected with. A page over its ceiling is a
// [*LimitError] naming [LimitPageSize]; see limits.go.
var (
	// ErrSearchQueryRequired: a search with nothing to look for is a
	// listing, and listing is what ListNodes is for.
	ErrSearchQueryRequired = errors.New("a search needs a query")
	// ErrSearchProjectNotFound wraps [ErrProjectNotFound] for a project
	// filter that resolves to nothing in the organization.
	ErrSearchProjectNotFound = fmt.Errorf("project: %w", ErrProjectNotFound)
)

// OpSearch is the action a hit is authorized as. It is never written to
// events, since a read changes nothing.
const OpSearch = "search"

// SearchQuery is one search. Query is required; everything else narrows
// the organization the search runs in.
type SearchQuery struct {
	// Query is what to look for, in PostgreSQL websearch syntax: bare
	// words, quoted phrases, `-` to exclude, and `or`.
	Query string
	// Project restricts hits to one project, named by id or by slug,
	// live or retired. Empty searches every project in the organization.
	Project string
	// IncludeArchived admits nodes of archived projects. Off by default:
	// archived work is out of the way unless the caller reaches for it.
	IncludeArchived bool
	// Limit caps the number of hits returned, at most [Limits.PageSize].
	// Zero takes that ceiling.
	Limit int
	// Offset skips that many hits of the ranked set, for the next page.
	Offset int
}

// Hit is one matching node, with the rank that ordered it and a snippet
// of the text that matched best.
type Hit struct {
	store.Node
	// Rank orders the page, best first. Comparable only within one
	// search.
	Rank float32 `json:"rank"`
	// Source is where the snippet comes from: `node` for the title or
	// body, `result`, or `comment`.
	Source string `json:"source"`
	// Snippet is a fragment of the matching text, each matched word
	// marked with <b> and </b>.
	Snippet string `json:"snippet"`
	// Ref is the node's reference, `slug#7`, from its project's current
	// slug. Hits span projects, so each carries its own.
	Ref string `json:"ref"`
}

// Search returns one page of the nodes matching q in the organization,
// best match first, that the actor may see under pol. A node is a hit
// when its title, body, a result, or a comment matches; each hit carries
// a snippet of the text that matched best. Order is by rank and then id,
// so the same rows page the same way twice.
//
// Hits the policy refuses are dropped after paging, so a page may come
// back short of q.Limit without being the last; a caller pages until a
// page is empty.
func Search(ctx context.Context, tx store.Tx, pol *policy.Bundle, orgID string, actor Actor, q SearchQuery) ([]Hit, error) {
	if strings.TrimSpace(q.Query) == "" {
		return nil, ErrSearchQueryRequired
	}
	if q.Offset < 0 {
		return nil, errors.New("offset is negative")
	}
	if err := actor.validate(); err != nil {
		return nil, err
	}
	pageSize, err := boundPageSize(q.Limit, ActiveLimits())
	if err != nil {
		return nil, err
	}
	params := store.SearchNodesParams{
		OrgID: orgID, Query: q.Query, IncludeArchived: q.IncludeArchived,
		PageSize: int32(pageSize), PageOffset: int32(q.Offset), //nolint:gosec // both bounded by the page ceiling and its caller
	}
	if q.Project != "" {
		project, err := searchProject(ctx, tx, orgID, q.Project)
		if err != nil {
			return nil, err
		}
		params.ProjectID = &project.ID
	}
	rows, err := tx.SearchNodes(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	r := &recorder{tx: tx, orgID: orgID, actor: actor, policy: pol}
	hits := make([]Hit, 0, len(rows))
	for _, row := range rows {
		visible, err := r.maySearch(ctx, row.Node)
		if err != nil {
			return nil, err
		}
		if visible {
			hits = append(hits, Hit{
				Node: row.Node, Rank: row.Rank, Source: row.Source, Snippet: row.Snippet,
				Ref: NodeRef{Project: row.ProjectSlug, Number: row.Node.Number}.String(),
			})
		}
	}
	return hits, nil
}

// maySearch asks the policy whether the actor may see node among search
// hits. A refusal is an answer, not an error; only a policy that could
// not decide is one.
func (r *recorder) maySearch(ctx context.Context, node store.Node) (bool, error) {
	info, err := r.nodeInfo(ctx, node)
	if err != nil {
		return false, err
	}
	in := policy.Input{
		Principal: policy.Principal{ID: r.actor.PrincipalID, Kind: r.actor.PrincipalKind},
		Action:    OpSearch,
		Resource:  policy.Resource{Node: info},
	}
	err = r.policy.Authorize(ctx, in)
	var denied *policy.DeniedError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &denied) && denied.Err == nil:
		return false, nil
	default:
		return false, fmt.Errorf("authorize search of node %s: %w", node.ID, err)
	}
}

// searchProject resolves a project filter: an id, or a slug live or
// retired. Ids are ULIDs and slugs are lowercase, so the two never
// collide.
func searchProject(ctx context.Context, tx store.Tx, orgID, ref string) (store.Project, error) {
	var (
		p   store.Project
		err error
	)
	if isULID(ref) {
		p, err = tx.GetProject(ctx, store.GetProjectParams{OrgID: orgID, ID: ref})
	} else {
		p, err = tx.GetProjectBySlug(ctx, store.GetProjectBySlugParams{OrgID: orgID, Slug: ref})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return p, fmt.Errorf("%s: %w", ref, ErrSearchProjectNotFound)
	}
	if err != nil {
		return p, fmt.Errorf("resolve project %s: %w", ref, err)
	}
	return p, nil
}

// isULID reports whether s is shaped like an id: 26 characters of the
// ULID alphabet with a leading character that keeps the timestamp in
// range, as the schema's is_ulid checks.
func isULID(s string) bool {
	if len(s) != 26 || s[0] > '7' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune(crockford, rune(s[i])) {
			return false
		}
	}
	return true
}
