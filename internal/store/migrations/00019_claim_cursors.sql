-- The rotation cursor a claim advances and a read follows, so both agree
-- on whose turn it is among the projects a filter matches. See "Claims
-- and reads select nodes by one filter, across every project" in
-- docs/domain-model.md's Priority section.
--
-- Keyed by the filter, not only the principal: a principal running two
-- independently-rotating claim loops over different filters must not
-- share one cursor, or one loop's turn would consume the other's.

-- +goose Up

CREATE TABLE claim_cursors (
    org_id       text        NOT NULL,
    principal_id text        NOT NULL,
    -- SHA-256 over the canonical encoding of the filter's scope-defining
    -- fields.
    filter_hash  text        NOT NULL,
    -- The project chosen last: the next claim or read starts after it.
    project_id   text        NOT NULL,
    updated_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT claim_cursors_pkey        PRIMARY KEY (org_id, principal_id, filter_hash),
    CONSTRAINT claim_cursors_org_id_fkey FOREIGN KEY (org_id) REFERENCES organizations (id)
);

-- +goose Down

DROP TABLE claim_cursors;
