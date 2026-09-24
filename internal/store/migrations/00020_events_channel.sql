-- The wake signal's channel. notify_event() raises skift_events, the name
-- internal/domain/wake.go listens on. See the Change stream section of
-- docs/domain-model.md.
--
-- The function is replaced here rather than edited in 00016 because a
-- migration a database has already applied is never run again: only a new
-- version reaches the trigger on an existing database.

-- +goose Up

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION notify_event() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('skift_events', NEW.org_id);
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION notify_event() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('skift_events', NEW.org_id);
    RETURN NULL;
END
$$;
-- +goose StatementEnd
