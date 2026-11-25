-- River's schema at its version 008, from github.com/riverqueue/river v0.51.2:
-- `river migrate-get --up --version 8` and `river migrate-get --down --version 8`,
-- copied unchanged. River's version 001, which creates its own migration
-- table, is left out: goose records skift's migrations, River's included.
-- See docs/adrs/016-background-jobs-and-admin.md.

-- +goose Up
-- +goose StatementBegin
-- River main migration 008 [up]
-- No-op. Postgres sequences already prevent automatically generated job IDs
-- from being reused.
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- River main migration 008 [down]
-- No-op. Postgres sequences already prevent automatically generated job IDs
-- from being reused.
SELECT 1;
-- +goose StatementEnd
