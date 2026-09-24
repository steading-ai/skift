package domain

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/steading-ai/skift/internal/store"
)

// ErrProjectNotFound is the sentinel a [*NotFoundError] for a project or
// organization unwraps to.
var ErrProjectNotFound = errors.New("project not found")

// ErrNoDefaultProject is returned when a bare `#7` is resolved without a
// project to read it in.
var ErrNoDefaultProject = errors.New("a bare #number reference needs a project")

// Ref is a parsed node reference. Exactly one of two shapes is set: a
// number, optionally qualified by project slug and organization, or an
// alias naming a node created earlier in the same patch. See the Keys and
// Forward references sections of docs/domain-model.md.
type Ref struct {
	// Org qualifies Project: `acme/oauth-spike#7`. Empty means the caller's
	// organization.
	Org string
	// Project is a slug, live or retired. Empty means the patch's project.
	Project string
	// Number is the node number within Project, from 1.
	Number int64
	// Alias is set instead of the above for an alias reference.
	Alias string
}

// IsAlias reports whether the reference names a node by patch alias.
func (r Ref) IsAlias() bool { return r.Alias != "" }

func (r Ref) String() string {
	if r.IsAlias() {
		return r.Alias
	}
	var b strings.Builder
	if r.Org != "" {
		b.WriteString(r.Org)
		b.WriteByte('/')
	}
	b.WriteString(r.Project)
	b.WriteByte('#')
	b.WriteString(strconv.FormatInt(r.Number, 10))
	return b.String()
}

// ParseError reports a reference that is not shaped like one.
type ParseError struct {
	Input  string
	Reason string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("malformed reference %q: %s", e.Input, e.Reason)
}

// ParseRef reads `#7`, `slug#7`, `org/slug#7`, or an alias. Anything
// containing `#` is a numbered reference; anything else is an alias. An
// alias may not contain `#`, `/`, or whitespace, so no alias can be
// mistaken for a numbered reference or a slug for an alias.
func ParseRef(s string) (Ref, error) {
	if s == "" {
		return Ref{}, &ParseError{Input: s, Reason: "empty"}
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return Ref{}, &ParseError{Input: s, Reason: "contains whitespace"}
	}
	hash := strings.IndexByte(s, '#')
	if hash < 0 {
		if strings.Contains(s, "/") {
			return Ref{}, &ParseError{Input: s, Reason: "an alias may not contain /"}
		}
		return Ref{Alias: s}, nil
	}

	qualifier, digits := s[:hash], s[hash+1:]
	if digits == "" || strings.TrimLeft(digits, "0123456789") != "" {
		return Ref{}, &ParseError{Input: s, Reason: "expected a number after #"}
	}
	number, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || number < 1 {
		return Ref{}, &ParseError{Input: s, Reason: "node numbers start at 1"}
	}
	ref := Ref{Number: number}

	if slash := strings.IndexByte(qualifier, '/'); slash >= 0 {
		ref.Org, ref.Project = qualifier[:slash], qualifier[slash+1:]
		if ref.Org == "" || ref.Project == "" || strings.Contains(ref.Project, "/") {
			return Ref{}, &ParseError{Input: s, Reason: "expected org/slug#number"}
		}
	} else {
		ref.Project = qualifier
	}
	return ref, nil
}

// Missing says which part of a reference did not resolve.
type Missing string

const (
	// MissingOrganization: the org qualifier is not the caller's
	// organization. Other organizations are invisible, so this is
	// indistinguishable from one that does not exist.
	MissingOrganization Missing = "organization"
	// MissingProject: no live or retired slug matches.
	MissingProject Missing = "project"
	// MissingNode: the project exists and has no such number.
	MissingNode Missing = "node"
	// MissingAlias: no create_node in the patch bound the alias.
	MissingAlias Missing = "alias"
)

// NotFoundError reports a reference that resolved to nothing, naming the
// part that failed. It unwraps to [ErrNodeNotFound] for a node or alias
// and to [ErrProjectNotFound] for a project or organization.
type NotFoundError struct {
	Ref     Ref
	Missing Missing
}

func (e *NotFoundError) Error() string {
	switch e.Missing {
	case MissingOrganization:
		return fmt.Sprintf("reference %s: organization %q not found", e.Ref, e.Ref.Org)
	case MissingProject:
		return fmt.Sprintf("reference %s: project %q not found", e.Ref, e.Ref.Project)
	case MissingAlias:
		return fmt.Sprintf("reference %s: alias %q not found in this patch", e.Ref, e.Ref.Alias)
	default:
		return fmt.Sprintf("reference %s: node %d not found in project %q", e.Ref, e.Ref.Number, e.Ref.Project)
	}
}

func (e *NotFoundError) Unwrap() error {
	switch e.Missing {
	case MissingOrganization, MissingProject:
		return ErrProjectNotFound
	default:
		return ErrNodeNotFound
	}
}

// Resolved is what a reference names. Title travels with it because
// numbers are dense: a mistyped number resolves to a real node, and the
// title is how a caller notices. See the Keys section of
// docs/domain-model.md.
type Resolved struct {
	ID        string
	ProjectID string
	Number    int64
	Title     string
}

// Resolver turns references into nodes within one transaction, for one
// organization, with the patch's project as the default for bare `#7` and
// the patch's aliases bound as its create_node operations are applied.
// Projects are looked up once per slug.
type Resolver struct {
	tx      store.Tx
	orgID   string
	project *store.Project
	org     *store.Organization
	slugs   map[string]*store.Project
	aliases map[string]Resolved
}

// NewResolver scopes resolution to orgID within tx. project is the patch's
// project, which bare `#7` reads in; nil makes such references an error.
func NewResolver(tx store.Tx, orgID string, project *store.Project) *Resolver {
	r := &Resolver{
		tx: tx, orgID: orgID, project: project,
		slugs:   map[string]*store.Project{},
		aliases: map[string]Resolved{},
	}
	if project != nil {
		r.slugs[project.Slug] = project
	}
	return r
}

// Bind makes alias name node for the rest of the patch. Binding a name
// twice is an error, since the second use would silently repoint every
// later reference.
func (r *Resolver) Bind(alias string, node store.Node) error {
	ref, err := ParseRef(alias)
	if err != nil {
		return err
	}
	if !ref.IsAlias() {
		return &ParseError{Input: alias, Reason: "an alias may not contain #"}
	}
	if _, dup := r.aliases[alias]; dup {
		return fmt.Errorf("alias %q is bound twice in one patch", alias)
	}
	r.aliases[alias] = Resolved{ID: node.ID, ProjectID: node.ProjectID, Number: node.Number, Title: node.Title}
	return nil
}

// Resolve parses and resolves one reference.
func (r *Resolver) Resolve(ctx context.Context, ref string) (Resolved, error) {
	parsed, err := ParseRef(ref)
	if err != nil {
		return Resolved{}, err
	}
	return r.ResolveRef(ctx, parsed)
}

// ResolveRef resolves a parsed reference. A failure is a [*NotFoundError]
// naming the part that was not found.
func (r *Resolver) ResolveRef(ctx context.Context, ref Ref) (Resolved, error) {
	if ref.IsAlias() {
		node, ok := r.aliases[ref.Alias]
		if !ok {
			return Resolved{}, &NotFoundError{Ref: ref, Missing: MissingAlias}
		}
		return node, nil
	}

	if ref.Org != "" {
		ok, err := r.isOwnOrg(ctx, ref.Org)
		if err != nil {
			return Resolved{}, err
		}
		if !ok {
			return Resolved{}, &NotFoundError{Ref: ref, Missing: MissingOrganization}
		}
	}

	project, err := r.projectFor(ctx, ref)
	if err != nil {
		return Resolved{}, err
	}

	node, err := r.tx.GetNodeByNumber(ctx, store.GetNodeByNumberParams{OrgID: r.orgID, ProjectID: project.ID, Number: ref.Number})
	if errors.Is(err, pgx.ErrNoRows) {
		return Resolved{}, &NotFoundError{Ref: ref, Missing: MissingNode}
	}
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve %s: %w", ref, err)
	}
	return Resolved{ID: node.ID, ProjectID: node.ProjectID, Number: node.Number, Title: node.Title}, nil
}

// isOwnOrg reports whether qualifier names the caller's organization, by
// id or by name. Any other organization is out of scope, so the answer is
// the same whether or not it exists.
func (r *Resolver) isOwnOrg(ctx context.Context, qualifier string) (bool, error) {
	if qualifier == r.orgID {
		return true, nil
	}
	if r.org == nil {
		org, err := r.tx.GetOrganization(ctx, r.orgID)
		if err != nil {
			return false, fmt.Errorf("resolve organization: %w", err)
		}
		r.org = &org
	}
	return qualifier == r.org.Name, nil
}

// projectFor finds the project a numbered reference reads in: the patch's
// project for a bare number, otherwise the slug, live or retired.
func (r *Resolver) projectFor(ctx context.Context, ref Ref) (*store.Project, error) {
	if ref.Project == "" {
		if r.project == nil {
			return nil, ErrNoDefaultProject
		}
		return r.project, nil
	}
	if p, ok := r.slugs[ref.Project]; ok {
		return p, nil
	}
	p, err := r.tx.GetProjectBySlug(ctx, store.GetProjectBySlugParams{OrgID: r.orgID, Slug: ref.Project})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, &NotFoundError{Ref: ref, Missing: MissingProject}
	}
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", ref, err)
	}
	r.slugs[ref.Project] = &p
	return &p, nil
}

// AllocateNumbers reserves count consecutive node numbers in the project
// and returns the first; the block is first through first+count-1. It is
// one statement per call however large the block, so a patch creating
// many nodes calls it once. The row lock the update takes serializes
// concurrent patches, and the counter only ever advances, so a number is
// never handed out twice and never reused after a drop. See the Keys
// section of docs/domain-model.md.
func AllocateNumbers(ctx context.Context, tx store.Tx, orgID, projectID string, count int) (int64, error) {
	if count < 1 {
		return 0, fmt.Errorf("allocate node numbers: count must be positive, got %d", count)
	}
	first, err := tx.AllocateNodeNumbers(ctx, store.AllocateNodeNumbersParams{OrgID: orgID, ID: projectID, Count: int64(count)})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrProjectNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("allocate node numbers: %w", err)
	}
	return int64(first), nil
}
