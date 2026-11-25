package main

// mutant is one deliberate break in the store's SQL: Old occurs exactly
// once in File, relative to the repository root, and New takes its place.
// Rule names what the change breaks, so a mutant that lives says which
// rule no spec holds.
type mutant struct {
	ID   string
	File string
	Old  string
	New  string
	Rule string
}

// mutants are the rules the store's SQL states, each broken once. Files
// ending .sql.gen.go are what sqlc generated from internal/store/queries:
// the SQL that runs is the string there, so a mutant edits that string,
// not the .sql file it came from. Add a row when a rule is added, and
// change one when its SQL is rewritten; the spec suite fails while a row's
// Old text no longer occurs exactly once.
var mutants = []mutant{
	{
		ID: "M01", File: "internal/store/ready.go",
		Old:  "AND (b.status = 'open' OR (b.assignee_kind = 'user' AND b.status <> 'done'))`",
		New:  "AND (b.status = 'open')`",
		Rule: "a dropped user gate stops blocking",
	},
	{
		ID: "M02", File: "internal/store/ready.go",
		Old:  "l.released_at IS NULL AND l.expires_at > now()`",
		New:  "l.released_at IS NULL`",
		Rule: "an expired lease keeps a node out of ready",
	},
	{
		ID: "M03", File: "internal/store/ready.go",
		Old:  "c.parent_id = n.id AND c.status = 'open'`",
		New:  "c.parent_id = n.id`",
		Rule: "closed children still block their parent",
	},
	{
		ID: "M04", File: "internal/store/ready.go",
		Old:  "p.id = n.project_id AND p.archived_at IS NOT NULL`",
		New:  "p.id = n.project_id AND false`",
		Rule: "archived projects stay claimable",
	},
	{
		ID: "M05", File: "internal/store/ready.go",
		Old:  "JOIN nodes p ON p.project_id = chain.project_id AND p.id = chain.parent_id",
		New:  "JOIN nodes p ON p.project_id = chain.project_id AND p.id = chain.parent_id AND false",
		Rule: "holds stop passing down containment",
	},
	{
		ID: "M06", File: "internal/store/ready.go",
		Old:  "p.id = n.project_id AND p.held_at IS NOT NULL`",
		New:  "p.id = n.project_id AND false`",
		Rule: "project holds stop parking work",
	},
	{
		ID: "M07", File: "internal/store/ready.go",
		Old:  `" ORDER BY n.priority, n.created_at, n.id"`,
		New:  `" ORDER BY n.priority, n.id"`,
		Rule: "ready order loses oldest-first",
	},
	{
		ID: "M08", File: "internal/store/ready.go",
		Old:  `" AND (n.assignee_kind IS NULL OR n.assignee_kind = 'agent')"`,
		New:  `" AND n.assignee_kind = 'agent'"`,
		Rule: "unassigned work drops out of agent filters",
	},
	{
		ID: "M09", File: "internal/store/ready.go",
		Old:  `where("n.labels @> $%d::text[]", f.Labels)`,
		New:  `where("n.labels && $%d::text[]", f.Labels)`,
		Rule: "the every-label filter becomes any-label",
	},
	{
		ID: "M10", File: "internal/store/ready.go",
		Old:  `where("n.priority <= $%d", f.MinPriority)`,
		New:  `where("n.priority < $%d", f.MinPriority)`,
		Rule: "the minimum-priority filter excludes its own priority",
	},
	{
		ID: "M11", File: "internal/store/ready.go",
		Old:  `sql+" FOR UPDATE OF n SKIP LOCKED"`,
		New:  `sql+" FOR UPDATE OF n"`,
		Rule: "concurrent claims wait instead of skipping",
	},
	{
		ID: "M12", File: "internal/store/ready.go",
		Old:  "n.project_id = $2 AND n.status = 'open' AND `",
		New:  "n.project_id = $2 AND `",
		Rule: "HeldCount counts closed nodes",
	},
	{
		ID: "M13", File: "internal/store/ready_holds.go",
		Old:  "WHERE s.held_at IS NOT NULL AND n.status = 'open'",
		New:  "WHERE false",
		Rule: "the hold summary forgets project holds",
	},
	{
		ID: "M14", File: "internal/store/ready_holds.go",
		Old:  "n.id = target.node_id AND n.status = 'open'",
		New:  "n.id = target.node_id",
		Rule: "the hold summary walks through closed nodes",
	},
	{
		ID: "M15", File: "internal/store/ready_holds.go",
		Old:  "c.id = r.node_id AND c.parent_id IS NOT NULL",
		New:  "c.id = r.node_id AND false",
		Rule: "the hold summary ignores child-to-parent waits",
	},
	{
		ID: "M16", File: "internal/store/edges.sql.gen.go",
		Old:  "WHERE c.org_id = $1 AND c.parent_id = r.node",
		New:  "WHERE c.org_id = $1 AND c.parent_id = r.node AND false",
		Rule: "the cycle check ignores containment",
	},
	{
		ID: "M17", File: "internal/store/leases.sql.gen.go",
		Old:  "l.released_at IS NULL AND l.expires_at > now()",
		New:  "l.released_at IS NULL",
		Rule: "the fence treats an expired lease as live",
	},
	{
		ID: "M18", File: "internal/store/node_list.go",
		Old:  `where("n.labels @> $%d::text[]", f.Labels)`,
		New:  `where("n.labels && $%d::text[]", f.Labels)`,
		Rule: "the node list's every-label filter becomes any-label",
	},
}
