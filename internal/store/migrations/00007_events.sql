-- Events: the audit record. See the Event and Change stream sections of
-- docs/domain-model.md and principle 7 in docs/core-principles.md.
--
-- Immutable, append-only, and time-partitioned from the start, because
-- retrofitting partitioning to a large table is painful (ADR-001). Rows are
-- kept indefinitely; what a partition window bounds is how far back a stream
-- consumer may resume, never what audit retains.

-- +goose Up

-- Refuses any rewrite of a row. Shared by every table whose rows are a
-- record of something that happened.
-- +goose StatementBegin
CREATE FUNCTION raise_append_only() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not permitted', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'restrict_violation';
END
$$;
-- +goose StatementEnd

CREATE TABLE events (
    -- ULID, so time-ordered. project_id and node_id are null for changes
    -- above that scope, such as creating a project.
    id              text        NOT NULL,
    org_id          text        NOT NULL,
    project_id      text,
    node_id         text,
    -- The actor, resolved by the transport. principal_kind is the kind at
    -- the time, so a later change to the principal does not rewrite history.
    principal_id    text        NOT NULL,
    principal_kind  text        NOT NULL,
    on_behalf_of    text,
    identity_scheme text        NOT NULL,
    -- The patch's idempotency key. Null for writes no client requested,
    -- such as the expiry timer releasing a lease.
    request_id      text,
    op              text        NOT NULL,
    reason          text,
    before          jsonb,
    after           jsonb,
    occurred_at     timestamptz NOT NULL DEFAULT now(),

    -- The partition key must be part of every unique constraint.
    CONSTRAINT events_pkey              PRIMARY KEY (id, occurred_at),
    CONSTRAINT events_org_id_fkey       FOREIGN KEY (org_id) REFERENCES organizations (id),
    CONSTRAINT events_project_fkey      FOREIGN KEY (org_id, project_id)
        REFERENCES projects (org_id, id),
    CONSTRAINT events_node_fkey         FOREIGN KEY (org_id, node_id)
        REFERENCES nodes (org_id, id),
    CONSTRAINT events_principal_fkey    FOREIGN KEY (org_id, principal_id)
        REFERENCES principals (org_id, id),
    CONSTRAINT events_on_behalf_of_fkey FOREIGN KEY (org_id, on_behalf_of)
        REFERENCES principals (org_id, id),
    CONSTRAINT events_id_ulid           CHECK (is_ulid(id)),
    CONSTRAINT events_principal_kind_check
        CHECK (principal_kind IN ('user', 'agent', 'service')),
    CONSTRAINT events_identity_scheme_nonempty CHECK (identity_scheme <> ''),
    CONSTRAINT events_op_nonempty       CHECK (op <> ''),
    -- A node lives in a project, so an event about one names both.
    CONSTRAINT events_node_needs_project CHECK (node_id IS NULL OR project_id IS NOT NULL)
) PARTITION BY RANGE (occurred_at);

-- The stream: everything in an organization after a cursor.
CREATE INDEX events_org_id_id_idx ON events (org_id, id);

-- Context recovery: the history of one node, in order.
CREATE INDEX events_node_id_id_idx ON events (node_id, id)
    WHERE node_id IS NOT NULL;

CREATE TRIGGER events_append_only
    BEFORE UPDATE OR DELETE ON events
    FOR EACH ROW EXECUTE FUNCTION raise_append_only();

CREATE TRIGGER events_no_truncate
    BEFORE TRUNCATE ON events
    FOR EACH STATEMENT EXECUTE FUNCTION raise_append_only();

-- Partitions are one calendar month each, in UTC, named events_YYYYMM.
-- ensure_event_partitions makes sure the partition holding `at` exists
-- along with `ahead` more after it, and returns the names it created.
-- Something must call it before a month arrives: the migration below does
-- so for now, and the service is expected to call it periodically.
-- +goose StatementBegin
CREATE FUNCTION ensure_event_partitions(at timestamptz DEFAULT now(), ahead integer DEFAULT 1)
    RETURNS SETOF text
    LANGUAGE plpgsql AS $$
DECLARE
    period_start timestamptz := date_trunc('month', at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC';
    period_end   timestamptz;
    partition    text;
BEGIN
    IF ahead < 0 THEN
        RAISE EXCEPTION 'ahead must not be negative: %', ahead;
    END IF;
    FOR i IN 0..ahead LOOP
        period_end := period_start + interval '1 month';
        partition  := 'events_' || to_char(period_start AT TIME ZONE 'UTC', 'YYYYMM');
        IF to_regclass(partition) IS NULL THEN
            EXECUTE format(
                'CREATE TABLE IF NOT EXISTS %I PARTITION OF events FOR VALUES FROM (%L) TO (%L)',
                partition, period_start, period_end);
            RETURN NEXT partition;
        END IF;
        period_start := period_end;
    END LOOP;
END
$$;
-- +goose StatementEnd

SELECT ensure_event_partitions();

-- +goose Down

DROP FUNCTION ensure_event_partitions(timestamptz, integer);
DROP TABLE events;
DROP FUNCTION raise_append_only();
