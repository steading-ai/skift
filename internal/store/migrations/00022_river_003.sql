-- River's schema at its version 003, from github.com/riverqueue/river v0.51.2:
-- `river migrate-get --up --version 3` and `river migrate-get --down --version 3`,
-- copied unchanged. River's version 001, which creates its own migration
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
-- River main migration 003 [up]
ALTER TABLE river_job ALTER COLUMN tags SET DEFAULT '{}';
UPDATE river_job SET tags = '{}' WHERE tags IS NULL;
ALTER TABLE river_job ALTER COLUMN tags SET NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- River main migration 003 [down]
ALTER TABLE river_job
    ALTER COLUMN tags DROP NOT NULL,
    ALTER COLUMN tags DROP DEFAULT;
-- +goose StatementEnd
