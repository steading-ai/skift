-- Projects and their slugs. See the Project and Keys sections of
-- docs/domain-model.md.
--
-- A project is a namespace for node numbers and an authorization scope, and
-- it must be free to create: no DDL, no sequence, no adjudication. The node
-- counter is therefore a plain column, and the slug is an alias over an
-- immutable id rather than the identity itself.

-- +goose Up

-- A slug is lowercase, at most 100 characters, and free of the characters
-- that delimit a node reference (`acme/oauth-spike#7`).
-- +goose StatementBegin
CREATE FUNCTION is_slug(candidate text) RETURNS boolean
    LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
    RETURN length(candidate) BETWEEN 1 AND 100
       AND candidate = lower(candidate)
       AND candidate !~ '[/#[:space:]]';
-- +goose StatementEnd

CREATE TABLE projects (
    id           text        NOT NULL,
    org_id       text        NOT NULL,
    slug         text        NOT NULL,
    title        text        NOT NULL,
    priority     text        NOT NULL DEFAULT 'p2',
    -- The next node number to hand out. Numbers start at 1 and are allocated
    -- a block at a time by reading and advancing this column in one
    -- statement.
    node_counter bigint      NOT NULL DEFAULT 1,
    archived_at  timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT projects_pkey            PRIMARY KEY (id),
    CONSTRAINT projects_org_id_slug_key UNIQUE (org_id, slug),
    -- Lets child tables reference (org_id, id) together, so a row can never
    -- claim a project from one organization and membership of another.
    CONSTRAINT projects_org_id_id_key   UNIQUE (org_id, id),
    CONSTRAINT projects_org_id_fkey     FOREIGN KEY (org_id) REFERENCES organizations (id),
    CONSTRAINT projects_id_ulid         CHECK (is_ulid(id)),
    CONSTRAINT projects_slug_shape      CHECK (is_slug(slug)),
    CONSTRAINT projects_title_nonempty  CHECK (title <> ''),
    CONSTRAINT projects_priority_check  CHECK (priority IN ('p0', 'p1', 'p2', 'p3', 'p4')),
    CONSTRAINT projects_node_counter_positive CHECK (node_counter >= 1)
);

-- Slugs a project no longer answers to but that still resolve to it. A slug
-- is unique per organization across both tables, so a reference written
-- down before a rename means the same thing after it.
CREATE TABLE project_slug_aliases (
    org_id     text        NOT NULL,
    project_id text        NOT NULL,
    slug       text        NOT NULL,
    retired_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT project_slug_aliases_pkey         PRIMARY KEY (org_id, slug),
    CONSTRAINT project_slug_aliases_project_fkey FOREIGN KEY (org_id, project_id)
        REFERENCES projects (org_id, id) ON DELETE CASCADE,
    CONSTRAINT project_slug_aliases_slug_shape   CHECK (is_slug(slug))
);

CREATE INDEX project_slug_aliases_project_id_idx ON project_slug_aliases (project_id);

-- Every slug an organization has ever used, live or retired, and the project
-- it resolves to. This is the lookup behind `slug#7`.
CREATE VIEW project_slugs AS
    SELECT org_id, id AS project_id, slug, NULL::timestamptz AS retired_at
      FROM projects
    UNION ALL
    SELECT org_id, project_id, slug, retired_at
      FROM project_slug_aliases;

-- Renaming a project retires its old slug as an alias in the same statement,
-- so no caller can forget to.
-- +goose StatementBegin
CREATE FUNCTION projects_retire_slug() RETURNS trigger
    LANGUAGE plpgsql
AS $$
BEGIN
    INSERT INTO project_slug_aliases (org_id, project_id, slug)
    VALUES (OLD.org_id, OLD.id, OLD.slug)
    ON CONFLICT ON CONSTRAINT project_slug_aliases_pkey
        DO UPDATE SET retired_at = now();
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER projects_retire_slug
    AFTER UPDATE OF slug ON projects
    FOR EACH ROW
    WHEN (OLD.slug IS DISTINCT FROM NEW.slug)
    EXECUTE FUNCTION projects_retire_slug();

-- A live slug may not shadow another project's retired one; the reference
-- would become ambiguous. A project taking back a slug it retired itself
-- simply reclaims it.
--
-- This runs AFTER the row lands so that a concurrent rename away from the
-- same slug, which the unique index makes this statement wait for, has
-- already committed its alias by the time the check runs.
-- +goose StatementBegin
CREATE FUNCTION projects_guard_slug() RETURNS trigger
    LANGUAGE plpgsql
AS $$
DECLARE
    holder text;
BEGIN
    DELETE FROM project_slug_aliases
     WHERE org_id = NEW.org_id AND slug = NEW.slug AND project_id = NEW.id;

    SELECT project_id INTO holder
      FROM project_slug_aliases
     WHERE org_id = NEW.org_id AND slug = NEW.slug;
    IF FOUND THEN
        RAISE unique_violation USING
            MESSAGE = format('slug %L is retired by project %s', NEW.slug, holder),
            CONSTRAINT = 'projects_org_id_slug_key',
            TABLE = 'projects';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER projects_guard_slug
    AFTER INSERT OR UPDATE OF slug ON projects
    FOR EACH ROW
    EXECUTE FUNCTION projects_guard_slug();

-- The mirror image: a retired slug may not be one some project holds live.
-- +goose StatementBegin
CREATE FUNCTION project_slug_aliases_guard_slug() RETURNS trigger
    LANGUAGE plpgsql
AS $$
DECLARE
    holder text;
BEGIN
    SELECT id INTO holder
      FROM projects
     WHERE org_id = NEW.org_id AND slug = NEW.slug;
    IF FOUND THEN
        RAISE unique_violation USING
            MESSAGE = format('slug %L is held by project %s', NEW.slug, holder),
            CONSTRAINT = 'project_slug_aliases_pkey',
            TABLE = 'project_slug_aliases';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER project_slug_aliases_guard_slug
    AFTER INSERT OR UPDATE OF slug ON project_slug_aliases
    FOR EACH ROW
    EXECUTE FUNCTION project_slug_aliases_guard_slug();

-- +goose Down

DROP VIEW project_slugs;
DROP TABLE project_slug_aliases;
DROP TABLE projects;
DROP FUNCTION project_slug_aliases_guard_slug();
DROP FUNCTION projects_guard_slug();
DROP FUNCTION projects_retire_slug();
DROP FUNCTION is_slug(text);
