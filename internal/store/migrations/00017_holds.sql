-- Holds: the stored decision to park work. See the Hold section of
-- docs/domain-model.md and docs/adrs/010-hold-is-eligibility-not-status.md.
--
-- A hold removes a node from the ready set without changing its status,
-- the way a live lease does. Only the hold itself is stored. Whether a
-- node is effectively held, because it or an ancestor or its project
-- carries one, is a walk at read time and is never written down the
-- subtree: principle 1 in docs/core-principles.md.

-- +goose Up

ALTER TABLE nodes
    ADD COLUMN held_at     timestamptz,
    ADD COLUMN held_by     text,
    ADD COLUMN held_reason text,
    -- The focus sweep that placed this hold, if a sweep did. A hold placed
    -- by hand carries none, which is what keeps a sweep from adopting it
    -- and what keeps clearing a sweep from releasing it.
    ADD COLUMN hold_group  text,

    ADD CONSTRAINT nodes_held_by_fkey FOREIGN KEY (org_id, held_by)
        REFERENCES principals (org_id, id),
    -- A hold is one fact: who and when travel together, and a hold with no
    -- reason is one nobody dares release.
    ADD CONSTRAINT nodes_hold_complete CHECK (
        (held_at IS NULL AND held_by IS NULL AND held_reason IS NULL)
     OR (held_at IS NOT NULL AND held_by IS NOT NULL AND held_reason <> '')),
    ADD CONSTRAINT nodes_hold_group_held CHECK (hold_group IS NULL OR held_at IS NOT NULL),
    ADD CONSTRAINT nodes_hold_group_ulid CHECK (hold_group IS NULL OR is_ulid(hold_group));

ALTER TABLE projects
    ADD COLUMN held_at     timestamptz,
    ADD COLUMN held_by     text,
    ADD COLUMN held_reason text,
    ADD COLUMN hold_group  text,

    ADD CONSTRAINT projects_held_by_fkey FOREIGN KEY (org_id, held_by)
        REFERENCES principals (org_id, id),
    ADD CONSTRAINT projects_hold_complete CHECK (
        (held_at IS NULL AND held_by IS NULL AND held_reason IS NULL)
     OR (held_at IS NOT NULL AND held_by IS NOT NULL AND held_reason <> '')),
    ADD CONSTRAINT projects_hold_group_held CHECK (hold_group IS NULL OR held_at IS NOT NULL),
    ADD CONSTRAINT projects_hold_group_ulid CHECK (hold_group IS NULL OR is_ulid(hold_group));

-- The project-hold condition of Ready: "is this node's project held?",
-- by the same (org_id, id) lookup the archived condition uses.
CREATE INDEX projects_held_idx ON projects (org_id, id) WHERE held_at IS NOT NULL;

-- A focus sweep is nothing but its holds, grouped: these serve finding the
-- active sweep in a scope and releasing it in one statement.
CREATE INDEX nodes_hold_group_idx ON nodes (org_id, hold_group) WHERE hold_group IS NOT NULL;
CREATE INDEX projects_hold_group_idx ON projects (org_id, hold_group) WHERE hold_group IS NOT NULL;

-- The upward walk needs no index of its own: its step is the
-- (project_id, id) lookup nodes_project_id_id_key already serves.

-- +goose Down

DROP INDEX projects_hold_group_idx;
DROP INDEX nodes_hold_group_idx;
DROP INDEX projects_held_idx;

ALTER TABLE projects
    DROP CONSTRAINT projects_hold_group_ulid,
    DROP CONSTRAINT projects_hold_group_held,
    DROP CONSTRAINT projects_hold_complete,
    DROP CONSTRAINT projects_held_by_fkey,
    DROP COLUMN hold_group,
    DROP COLUMN held_reason,
    DROP COLUMN held_by,
    DROP COLUMN held_at;

ALTER TABLE nodes
    DROP CONSTRAINT nodes_hold_group_ulid,
    DROP CONSTRAINT nodes_hold_group_held,
    DROP CONSTRAINT nodes_hold_complete,
    DROP CONSTRAINT nodes_held_by_fkey,
    DROP COLUMN hold_group,
    DROP COLUMN held_reason,
    DROP COLUMN held_by,
    DROP COLUMN held_at;
