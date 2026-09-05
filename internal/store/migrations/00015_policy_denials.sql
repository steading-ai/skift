-- Policy denials: the audit record of refused operations. See the
-- Authorization section of docs/architecture.md and
-- docs/adrs/004-embedded-opa-authorization.md.
--
-- A denial rolls back the transaction that asked, so it cannot be recorded
-- in that transaction, and it is not a mutation, so it is not an event. It
-- gets its own append-only table, written after the rollback. node_id has
-- no foreign key: the node a denied operation named may have existed only
-- in the transaction that rolled back.

-- +goose Up

CREATE TABLE policy_denials (
    id              text        NOT NULL,
    org_id          text        NOT NULL,
    project_id      text,
    node_id         text,
    -- The actor, as the transport resolved it.
    principal_id    text        NOT NULL,
    principal_kind  text        NOT NULL,
    identity_scheme text        NOT NULL,
    -- The request's idempotency key. Null for writes no client requested.
    request_id      text,
    -- The operation the policy refused, as events name operations.
    op              text        NOT NULL,
    -- Why: the reasons the policy gave, or the evaluation failure that
    -- stood in for a decision.
    reasons         text[]      NOT NULL,
    -- The bundle that refused it. Empty when no bundle was loaded, which
    -- is itself a denial.
    policy_version  text        NOT NULL,
    occurred_at     timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT policy_denials_pkey            PRIMARY KEY (id),
    CONSTRAINT policy_denials_org_id_fkey     FOREIGN KEY (org_id) REFERENCES organizations (id),
    CONSTRAINT policy_denials_principal_fkey  FOREIGN KEY (org_id, principal_id)
        REFERENCES principals (org_id, id),
    CONSTRAINT policy_denials_id_ulid         CHECK (is_ulid(id)),
    CONSTRAINT policy_denials_principal_kind_check
        CHECK (principal_kind IN ('user', 'agent', 'service')),
    CONSTRAINT policy_denials_identity_scheme_nonempty CHECK (identity_scheme <> ''),
    CONSTRAINT policy_denials_op_nonempty     CHECK (op <> '')
);

-- The audit: what one organization was refused, in order.
CREATE INDEX policy_denials_org_id_id_idx ON policy_denials (org_id, id);

CREATE TRIGGER policy_denials_append_only
    BEFORE UPDATE OR DELETE ON policy_denials
    FOR EACH ROW EXECUTE FUNCTION raise_append_only();

CREATE TRIGGER policy_denials_no_truncate
    BEFORE TRUNCATE ON policy_denials
    FOR EACH STATEMENT EXECUTE FUNCTION raise_append_only();

-- +goose Down

DROP TABLE policy_denials;
