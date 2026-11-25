-- River's schema at its version 004, from github.com/riverqueue/river v0.51.2:
-- `river migrate-get --up --version 4` and `river migrate-get --down --version 4`,
-- copied unchanged except one statement pair in the Down section, explained
-- there. River's version 001, which creates its own migration
-- table, is left out: goose records skift's migrations, River's included.
-- See docs/adrs/016-background-jobs-and-admin.md.
--
-- squawk would flag River's statements as unsafe on a populated table. They
-- are not here: skift v0.4.0 creates every River table in these same
-- migrations, so each statement runs on an empty table. A later River
-- upgrade runs on populated tables and is linted in full.
-- squawk-ignore-file

-- +goose Up
-- +goose StatementBegin
-- River main migration 004 [up]
-- The args column never had a NOT NULL constraint or default value at the
-- database level, though we tried to ensure one at the application level.
ALTER TABLE river_job ALTER COLUMN args SET DEFAULT '{}';
UPDATE river_job SET args = '{}' WHERE args IS NULL;
ALTER TABLE river_job ALTER COLUMN args SET NOT NULL;
ALTER TABLE river_job ALTER COLUMN args DROP DEFAULT;

-- The metadata column never had a NOT NULL constraint or default value at the
-- database level, though we tried to ensure one at the application level.
ALTER TABLE river_job ALTER COLUMN metadata SET DEFAULT '{}';
UPDATE river_job SET metadata = '{}' WHERE metadata IS NULL;
ALTER TABLE river_job ALTER COLUMN metadata SET NOT NULL;

-- The 'pending' job state will be used for upcoming functionality:
ALTER TYPE river_job_state ADD VALUE IF NOT EXISTS 'pending' AFTER 'discarded';

ALTER TABLE river_job DROP CONSTRAINT finalized_or_finalized_at_null;
ALTER TABLE river_job ADD CONSTRAINT finalized_or_finalized_at_null CHECK (
    (finalized_at IS NULL AND state NOT IN ('cancelled', 'completed', 'discarded')) OR
    (finalized_at IS NOT NULL AND state IN ('cancelled', 'completed', 'discarded'))
);

DROP TRIGGER river_notify ON river_job;
DROP FUNCTION river_job_notify;

--
-- Create table `river_queue`.
--

CREATE TABLE river_queue (
    name text PRIMARY KEY NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    metadata jsonb NOT NULL DEFAULT '{}' ::jsonb,
    paused_at timestamptz,
    updated_at timestamptz NOT NULL
);

--
-- Alter `river_leader` to add a default value of 'default` to `name`.
--

ALTER TABLE river_leader
    ALTER COLUMN name SET DEFAULT 'default',
    DROP CONSTRAINT name_length,
    ADD CONSTRAINT name_length CHECK (name = 'default');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- River main migration 004 [down]
ALTER TABLE river_job ALTER COLUMN args DROP NOT NULL;

-- skift: River's own down here drops metadata's NOT NULL and default.
-- River's version 002 created the column with both, and this version's up
-- only re-applied them, so dropping them would leave the column below what
-- the up found. Skift's downs restore exactly what their up found, so
-- this one leaves the column as it is.

-- It is not possible to safely remove 'pending' from the river_job_state enum,
-- so leave it in place.

ALTER TABLE river_job DROP CONSTRAINT finalized_or_finalized_at_null;
ALTER TABLE river_job ADD CONSTRAINT finalized_or_finalized_at_null CHECK (
  (state IN ('cancelled', 'completed', 'discarded') AND finalized_at IS NOT NULL) OR finalized_at IS NULL
);

CREATE OR REPLACE FUNCTION river_job_notify()
  RETURNS TRIGGER
  AS $$
DECLARE
  payload json;
BEGIN
  IF NEW.state = 'available' THEN
    -- Notify will coalesce duplicate notifications within a transaction, so
    -- keep these payloads generalized:
    payload = json_build_object('queue', NEW.queue);
    PERFORM
      pg_notify('river_insert', payload::text);
  END IF;
  RETURN NULL;
END;
$$
LANGUAGE plpgsql;

CREATE TRIGGER river_notify
  AFTER INSERT ON river_job
  FOR EACH ROW
  EXECUTE PROCEDURE river_job_notify();

DROP TABLE river_queue;

ALTER TABLE river_leader
    ALTER COLUMN name DROP DEFAULT,
    DROP CONSTRAINT name_length,
    ADD CONSTRAINT name_length CHECK (char_length(name) > 0 AND char_length(name) < 128);
-- +goose StatementEnd
