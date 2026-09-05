-- Nodes: the unit of work. See the Node, Keys, and Readiness sections of
-- docs/domain-model.md.
--
-- Only what cannot be derived is stored. There is no blocked or ready
-- column: both are anti-joins over edges, leases, and children, and the
-- indexes here are what make those anti-joins cheap. See principle 1 in
-- docs/core-principles.md.

-- +goose Up

CREATE TABLE nodes (
    id            text        NOT NULL,
    -- Sequential within the project, from 1. Allocated from
    -- projects.node_counter, never from a sequence.
    number        bigint      NOT NULL,
    org_id        text        NOT NULL,
    project_id    text        NOT NULL,
    -- Containment, not dependency. A parent lives in the same project,
    -- because nodes never move between projects.
    parent_id     text,
    kind          text        NOT NULL DEFAULT 'task',
    title         text        NOT NULL,
    body          text        NOT NULL DEFAULT '',
    status        text        NOT NULL DEFAULT 'open',
    priority      text        NOT NULL DEFAULT 'p2',
    -- NULL means agent-eligible. 'user' makes the node a gate.
    assignee_kind text,
    assignee      text,
    labels        text[]      NOT NULL DEFAULT '{}',
    -- Minted here at creation and shared by every attempt on the node.
    context_id    uuid        NOT NULL DEFAULT gen_random_uuid(),
    created_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT nodes_pkey                   PRIMARY KEY (id),
    CONSTRAINT nodes_project_id_number_key  UNIQUE (project_id, number),
    -- Let edges and leases carry the organization and reference it
    -- together with the node, and let a parent be pinned to its child's
    -- project, so no row can straddle two tenancy scopes.
    CONSTRAINT nodes_org_id_id_key          UNIQUE (org_id, id),
    CONSTRAINT nodes_project_id_id_key      UNIQUE (project_id, id),
    CONSTRAINT nodes_project_fkey           FOREIGN KEY (org_id, project_id)
        REFERENCES projects (org_id, id),
    CONSTRAINT nodes_parent_fkey            FOREIGN KEY (project_id, parent_id)
        REFERENCES nodes (project_id, id),
    CONSTRAINT nodes_assignee_fkey          FOREIGN KEY (org_id, assignee)
        REFERENCES principals (org_id, id),
    CONSTRAINT nodes_id_ulid                CHECK (is_ulid(id)),
    CONSTRAINT nodes_number_positive        CHECK (number >= 1),
    CONSTRAINT nodes_not_own_parent         CHECK (parent_id <> id),
    CONSTRAINT nodes_kind_nonempty          CHECK (kind <> ''),
    CONSTRAINT nodes_title_nonempty         CHECK (title <> ''),
    CONSTRAINT nodes_status_check           CHECK (status IN ('open', 'done', 'dropped')),
    CONSTRAINT nodes_priority_check         CHECK (priority IN ('p0', 'p1', 'p2', 'p3', 'p4')),
    CONSTRAINT nodes_assignee_kind_check    CHECK (assignee_kind IN ('agent', 'user')),
    -- A specific assignee implies a kind, since the kind is the principal's.
    CONSTRAINT nodes_assignee_needs_kind    CHECK (assignee IS NULL OR assignee_kind IS NOT NULL)
);

-- The outer scan of Ready: open nodes of one project in the order Ready
-- returns them, priority then age. Priorities are 'p0'..'p4', so text
-- order is priority order.
CREATE INDEX nodes_open_by_project_idx ON nodes (project_id, priority, created_at)
    WHERE status = 'open';

-- The open-children anti-join: "does any open node have this parent?"
CREATE INDEX nodes_open_children_idx ON nodes (parent_id)
    WHERE status = 'open' AND parent_id IS NOT NULL;

-- Everything under a parent, open or not, for rollups and listing.
CREATE INDEX nodes_parent_id_idx ON nodes (parent_id)
    WHERE parent_id IS NOT NULL;

-- Lookups by the specific principal a node is assigned to.
CREATE INDEX nodes_assignee_idx ON nodes (org_id, assignee)
    WHERE assignee IS NOT NULL;

-- +goose Down

DROP TABLE nodes;
