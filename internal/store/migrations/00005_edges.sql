-- Edges: `from` blocks `to`. See the Edge section of docs/domain-model.md.
--
-- Both ends carry the organization, so an edge can only join nodes of one
-- tenant. It may cross projects. Acyclicity is a property of the whole
-- patch, checked in the domain layer under an advisory lock, not here.

-- +goose Up

CREATE TABLE edges (
    org_id     text        NOT NULL,
    from_node  text        NOT NULL,
    to_node    text        NOT NULL,
    -- Only 'blocks' affects readiness. Other kinds are annotations.
    kind       text        NOT NULL DEFAULT 'blocks',
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT edges_pkey           PRIMARY KEY (from_node, to_node, kind),
    CONSTRAINT edges_from_node_fkey FOREIGN KEY (org_id, from_node)
        REFERENCES nodes (org_id, id),
    CONSTRAINT edges_to_node_fkey   FOREIGN KEY (org_id, to_node)
        REFERENCES nodes (org_id, id),
    CONSTRAINT edges_not_self       CHECK (from_node <> to_node),
    CONSTRAINT edges_kind_nonempty  CHECK (kind <> '')
);

-- The readiness anti-join: "is there a blocks edge into this node whose
-- source is open?" The primary key leads with from_node and cannot answer
-- it.
CREATE INDEX edges_to_node_idx ON edges (to_node, kind, from_node);

-- +goose Down

DROP TABLE edges;
