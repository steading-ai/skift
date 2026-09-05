-- Results: what completing a node produced. See Completion and results in
-- docs/domain-model.md.
--
-- A result is a markdown body under the size cap; the artifacts that go
-- with it attach to the same node. A node completed, reopened, and
-- completed again keeps every result, and its current result is the most
-- recent, derived rather than stored.

-- +goose Up

CREATE TABLE results (
    id           text        NOT NULL,
    org_id       text        NOT NULL,
    node_id      text        NOT NULL,
    body         text        NOT NULL,
    completed_by text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT results_pkey              PRIMARY KEY (id),
    CONSTRAINT results_node_fkey         FOREIGN KEY (org_id, node_id)
        REFERENCES nodes (org_id, id),
    CONSTRAINT results_completed_by_fkey FOREIGN KEY (org_id, completed_by)
        REFERENCES principals (org_id, id),
    CONSTRAINT results_id_ulid           CHECK (is_ulid(id))
);

-- A node's results, newest first: the current one is the first row.
CREATE INDEX results_node_id_created_at_idx ON results (node_id, created_at DESC, id DESC);

-- +goose Down

DROP TABLE results;
