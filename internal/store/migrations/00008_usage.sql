-- Usage: what work cost. See the Usage section of docs/domain-model.md.
--
-- Append-only. An estimate and an actual are rows of the same shape with
-- different kinds, and a node's current estimate is its most recent one,
-- derived at query time rather than stored on the node.

-- +goose Up

-- Lets a usage row name its lease and its node together, so an actual can
-- never be attributed to a lease on some other node.
ALTER TABLE leases ADD CONSTRAINT leases_node_id_id_key UNIQUE (node_id, id);

CREATE TABLE usage (
    id                 text        NOT NULL,
    org_id             text        NOT NULL,
    node_id            text        NOT NULL,
    -- The attempt. Required on an actual; an estimate precedes any attempt.
    lease_id           text,
    -- Who reported the actual, or who made the estimate.
    principal          text        NOT NULL,
    model              text,
    -- Counts, never currency. Rates are applied at query time.
    input_tokens       bigint      NOT NULL DEFAULT 0,
    output_tokens      bigint      NOT NULL DEFAULT 0,
    cache_read_tokens  bigint      NOT NULL DEFAULT 0,
    cache_write_tokens bigint      NOT NULL DEFAULT 0,
    kind               text        NOT NULL,
    -- Estimates only: what the estimate was reasoned from.
    basis              text,
    created_at         timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT usage_pkey            PRIMARY KEY (id),
    CONSTRAINT usage_node_fkey       FOREIGN KEY (org_id, node_id)
        REFERENCES nodes (org_id, id),
    CONSTRAINT usage_lease_fkey      FOREIGN KEY (node_id, lease_id)
        REFERENCES leases (node_id, id),
    CONSTRAINT usage_principal_fkey  FOREIGN KEY (org_id, principal)
        REFERENCES principals (org_id, id),
    CONSTRAINT usage_id_ulid         CHECK (is_ulid(id)),
    CONSTRAINT usage_kind_check      CHECK (kind IN ('actual', 'estimate')),
    CONSTRAINT usage_tokens_nonnegative CHECK (
        input_tokens >= 0 AND output_tokens >= 0
        AND cache_read_tokens >= 0 AND cache_write_tokens >= 0),
    CONSTRAINT usage_actual_has_lease   CHECK (kind <> 'actual' OR lease_id IS NOT NULL),
    CONSTRAINT usage_basis_on_estimate  CHECK (basis IS NULL OR kind = 'estimate')
);

-- A node's usage by kind, newest first: the current estimate is the first
-- estimate row, and rollups read the actuals.
CREATE INDEX usage_node_id_kind_created_at_idx ON usage (node_id, kind, created_at DESC, id DESC);

CREATE TRIGGER usage_append_only
    BEFORE UPDATE OR DELETE ON usage
    FOR EACH ROW EXECUTE FUNCTION raise_append_only();

CREATE TRIGGER usage_no_truncate
    BEFORE TRUNCATE ON usage
    FOR EACH STATEMENT EXECUTE FUNCTION raise_append_only();

-- +goose Down

DROP TABLE usage;
ALTER TABLE leases DROP CONSTRAINT leases_node_id_id_key;
