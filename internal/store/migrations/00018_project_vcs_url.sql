-- The repository a project delivers to. See the Project section of
-- docs/domain-model.md.
--
-- Dagger stores the URL for the orchestrators that work the project and
-- never reads it: the column is an annotation, not a fifth thing a project
-- is. Empty is the default, so every existing project stays valid.

-- +goose Up

ALTER TABLE projects
    ADD COLUMN vcs_url text NOT NULL DEFAULT '';

-- +goose Down

ALTER TABLE projects
    DROP COLUMN vcs_url;
