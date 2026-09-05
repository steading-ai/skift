package domain

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/ivy/dagger/internal/policy"
	"github.com/ivy/dagger/internal/store"
)

// Projects are free to create: no provisioning, no adjudication of names.
// The slug is derived from the title and disambiguated on collision, and a
// retired slug counts as a collision because it still resolves. See the
// Project and Keys sections of docs/domain-model.md.

// Project operation names, as written to events.op.
const (
	OpCreateProject  = "create_project"
	OpArchiveProject = "archive_project"
)

// ErrTitleRequired: a project needs a title to derive its slug from.
var ErrTitleRequired = errors.New("a project needs a title")

// maxSlugLength is the ceiling the schema's is_slug enforces.
const maxSlugLength = 100

// fallbackSlug is used when a title yields no slug characters at all.
const fallbackSlug = "project"

// NewProject is what [Service.CreateProject] takes.
type NewProject struct {
	// RequestID is the client-supplied idempotency key.
	RequestID string `json:"request_id"`
	// Title is the human-readable name. Required.
	Title string `json:"title"`
	// Slug is the requested alias. Empty derives one from the title.
	// Either way it is normalized and disambiguated on collision.
	Slug string `json:"slug,omitempty"`
	// Priority is advisory, `p0` through `p4`. Empty means p2.
	Priority string `json:"priority,omitempty"`
}

// ProjectListQuery is one page of an organization's projects.
type ProjectListQuery struct {
	// IncludeArchived admits archived projects. Off by default.
	IncludeArchived bool
	// Limit caps the page, at most [Limits.PageSize]. Zero takes that
	// ceiling.
	Limit int
	// Offset skips that many projects, for the next page.
	Offset int
}

// CreateProject creates a project for actor, idempotently under its
// request id, and records the creation as an event. The slug is the
// requested one or one derived from the title, lowercased, and made unique
// among the organization's live and retired slugs with a numeric suffix.
func (g *Service) CreateProject(ctx context.Context, orgID string, actor Actor, in NewProject) (*Project, error) {
	if in.RequestID == "" {
		return nil, ErrNoRequestID
	}
	if err := actor.validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Title) == "" {
		return nil, ErrTitleRequired
	}
	if in.Priority == "" {
		in.Priority = "p2"
	}
	if !validPriority(in.Priority) {
		return nil, fmt.Errorf("priority %q is not p0 through p4", in.Priority)
	}
	hash, err := requestHash(in)
	if err != nil {
		return nil, err
	}
	base := slugify(in.Title)
	if in.Slug != "" {
		base = normalizeSlug(in.Slug)
	}

	var project *store.Project
	for attempt := 0; ; attempt++ {
		err := g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			created, _, err := withRequest(ctx, tx, orgID, in.RequestID, hash, func(ctx context.Context) (*store.Project, error) {
				if err := lockProjects(ctx, tx, orgID); err != nil {
					return nil, err
				}
				slug, err := freeSlug(ctx, tx, orgID, base)
				if err != nil {
					return nil, err
				}
				p, err := tx.CreateProject(ctx, store.CreateProjectParams{
					ID: newULID(), OrgID: orgID, Slug: slug, Title: in.Title, Priority: in.Priority,
				})
				if err != nil {
					return nil, fmt.Errorf("create project %q: %w", slug, err)
				}
				if err := projectEvent(ctx, tx, g.pol, orgID, actor, &in.RequestID, OpCreateProject, nil, p); err != nil {
					return nil, err
				}
				return &p, nil
			})
			project = created
			return err
		})
		if isKeyConflict(err) && attempt == 0 {
			continue
		}
		if err != nil {
			return nil, auditDenial(ctx, g.s, orgID, actor, &in.RequestID, err)
		}
		return project, nil
	}
}

// GetProject returns the project ref names: a slug, live or retired, an
// `org/slug`, or an id.
func (g *Service) GetProject(ctx context.Context, orgID, ref string) (*Project, error) {
	if ref == "" {
		return nil, ErrProjectRequired
	}
	var project *store.Project
	err := g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
		project, err = resolveProject(ctx, tx, orgID, ref)
		return err
	})
	return project, err
}

// ListProjects returns one page of the organization's projects, most
// recently created first. Archived projects are left out unless asked for.
func (g *Service) ListProjects(ctx context.Context, orgID string, q ProjectListQuery) ([]Project, error) {
	if q.Offset < 0 {
		return nil, errors.New("offset is negative")
	}
	pageSize, err := boundPageSize(q.Limit, ActiveLimits())
	if err != nil {
		return nil, err
	}
	var projects []store.Project
	err = g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
		projects, err = tx.ListProjects(ctx, store.ListProjectsParams{
			OrgID: orgID, IncludeArchived: q.IncludeArchived,
			PageSize: int32(pageSize), PageOffset: int32(q.Offset), //nolint:gosec // bounded by the page ceiling and its caller
		})
		if err != nil {
			return fmt.Errorf("list projects: %w", err)
		}
		return nil
	})
	return projects, err
}

// ArchiveProject makes the project read-only, idempotently under its
// request id. Archiving an archived project changes nothing and records
// nothing.
func (g *Service) ArchiveProject(ctx context.Context, orgID string, actor Actor, requestID, ref string) (*Project, error) {
	if requestID == "" {
		return nil, ErrNoRequestID
	}
	if err := actor.validate(); err != nil {
		return nil, err
	}
	if ref == "" {
		return nil, ErrProjectRequired
	}
	hash, err := requestHash(struct {
		Op      string `json:"op"`
		Project string `json:"project"`
	}{OpArchiveProject, ref})
	if err != nil {
		return nil, err
	}

	var project *store.Project
	for attempt := 0; ; attempt++ {
		err := g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			archived, _, err := withRequest(ctx, tx, orgID, requestID, hash, func(ctx context.Context) (*store.Project, error) {
				before, err := resolveProject(ctx, tx, orgID, ref)
				if err != nil {
					return nil, err
				}
				if before.ArchivedAt != nil {
					return before, nil
				}
				after, err := tx.ArchiveProject(ctx, store.ArchiveProjectParams{OrgID: orgID, ID: before.ID})
				if err != nil {
					return nil, fmt.Errorf("archive project %s: %w", before.Slug, err)
				}
				if err := projectEvent(ctx, tx, g.pol, orgID, actor, &requestID, OpArchiveProject, before, after); err != nil {
					return nil, err
				}
				return &after, nil
			})
			project = archived
			return err
		})
		if isKeyConflict(err) && attempt == 0 {
			continue
		}
		if err != nil {
			return nil, auditDenial(ctx, g.s, orgID, actor, &requestID, err)
		}
		return project, nil
	}
}

// projectEvent authorizes a project operation and writes its event. A
// project event names no node.
func projectEvent(ctx context.Context, tx store.Tx, pol *policy.Bundle, orgID string, actor Actor, requestID *string, op string, before, after any) error {
	in := policy.Input{Principal: policy.Principal{ID: actor.PrincipalID, Kind: actor.PrincipalKind}, Action: op}
	if err := pol.Authorize(ctx, in); err != nil {
		denial := &Denial{Op: op, PolicyVersion: pol.Version()}
		if !errors.As(err, &denial.Err) {
			denial.Err = &policy.DeniedError{Action: op, Err: err}
		}
		if p, ok := after.(store.Project); ok {
			denial.ProjectID = p.ID
		}
		return denial
	}
	var projectID string
	if p, ok := after.(store.Project); ok {
		projectID = p.ID
	}
	beforeJSON, err := encodeChange(before)
	if err != nil {
		return err
	}
	afterJSON, err := encodeChange(after)
	if err != nil {
		return err
	}
	if _, err := tx.CreateEvent(ctx, store.CreateEventParams{
		ID: newULID(), OrgID: orgID, ProjectID: &projectID,
		PrincipalID: actor.PrincipalID, PrincipalKind: actor.PrincipalKind, IdentityScheme: actor.IdentityScheme,
		RequestID: requestID, Op: op, Before: beforeJSON, After: afterJSON, PolicyVersion: pol.Version(),
	}); err != nil {
		return fmt.Errorf("record %s event: %w", op, err)
	}
	return nil
}

// projectsLockNamespace keys the advisory lock slug allocation takes, apart
// from the graph lock so a project creation never waits on a patch.
const projectsLockNamespace = "dagger:projects"

// lockProjects serializes slug allocation within the organization for the
// rest of the transaction, so two creations with one title cannot both find
// the same slug free.
func lockProjects(ctx context.Context, tx store.Tx, orgID string) error {
	if _, err := tx.Tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`, projectsLockNamespace, orgID); err != nil {
		return fmt.Errorf("lock organization projects: %w", err)
	}
	return nil
}

// freeSlug returns base, or base with the lowest numeric suffix from 2 that
// no live or retired slug of the organization carries.
func freeSlug(ctx context.Context, tx store.Tx, orgID, base string) (string, error) {
	for n := 1; ; n++ {
		candidate := base
		if n > 1 {
			suffix := "-" + strconv.Itoa(n)
			candidate = truncateRunes(base, maxSlugLength-len(suffix)) + suffix
		}
		_, err := tx.GetProjectBySlug(ctx, store.GetProjectBySlugParams{OrgID: orgID, Slug: candidate})
		if errors.Is(err, pgx.ErrNoRows) {
			return candidate, nil
		}
		if err != nil {
			return "", fmt.Errorf("check slug %q: %w", candidate, err)
		}
	}
}

// slugify derives a slug from a title: lowercase, letters and digits kept,
// every other run of characters folded to one hyphen, bounded to the
// schema's length.
func slugify(title string) string {
	var b strings.Builder
	pending := false
	for _, r := range strings.ToLower(title) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if pending && b.Len() > 0 {
				b.WriteByte('-')
			}
			pending = false
			b.WriteRune(r)
			continue
		}
		pending = true
	}
	return finishSlug(b.String())
}

// normalizeSlug makes a requested slug acceptable to the schema with the
// least change: lowercase, and the characters that delimit a node
// reference folded to hyphens.
func normalizeSlug(slug string) string {
	var b strings.Builder
	pending := false
	for _, r := range strings.ToLower(slug) {
		if r == '/' || r == '#' || unicode.IsSpace(r) {
			pending = true
			continue
		}
		if pending && b.Len() > 0 {
			b.WriteByte('-')
		}
		pending = false
		b.WriteRune(r)
	}
	return finishSlug(b.String())
}

func finishSlug(s string) string {
	s = strings.Trim(truncateRunes(s, maxSlugLength), "-")
	if s == "" {
		return fallbackSlug
	}
	return s
}

func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
