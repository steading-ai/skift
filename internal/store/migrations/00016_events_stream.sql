-- The change stream over events. See the Change stream section of
-- docs/domain-model.md.
--
-- An event id is a ULID minted before commit, so id order is not commit
-- order: a transaction can take a low id, stall, and commit after a higher
-- id has been delivered. The stream therefore positions itself by
-- transaction id instead. Every transaction id below a snapshot's xmin has
-- finished, so a cursor that carries that boundary, plus the ids it has
-- delivered above it, never skips a late commit.

-- +goose Up

-- The writing transaction's id, as pg_current_xact_id() reports it. A
-- bigint rather than xid8 so the generated query set handles it as a
-- plain integer; xid8 has no direct cast to bigint, hence the text step.
ALTER TABLE events
    ADD COLUMN tx_id bigint NOT NULL DEFAULT (pg_current_xact_id()::text::bigint);

-- The stream: everything in an organization from a transaction boundary,
-- in transaction and then id order.
CREATE INDEX events_org_id_tx_id_id_idx ON events (org_id, tx_id, id);

-- The wake signal. The payload names the organization something changed
-- in and nothing more: a consumer that hears it reads the events table
-- from its cursor. Notifications are delivered at commit and identical
-- ones within a transaction collapse, so a patch of many operations wakes
-- a consumer once.
-- +goose StatementBegin
CREATE FUNCTION notify_event() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('dagger_events', NEW.org_id);
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER events_notify
    AFTER INSERT ON events
    FOR EACH ROW EXECUTE FUNCTION notify_event();

-- +goose Down

DROP TRIGGER events_notify ON events;
DROP FUNCTION notify_event();
DROP INDEX events_org_id_tx_id_id_idx;
ALTER TABLE events DROP COLUMN tx_id;
