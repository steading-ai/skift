-- The schema's compatibility floor: the lowest migration version a binary
-- must carry to serve this schema. A binary whose newest migration is at
-- least the floor serves the schema, even one newer than itself, and any
-- other refuses it. See docs/adrs/018-unattended-upgrades.md.
--
-- It lives in the database because an older binary cannot read migrations
-- it does not carry. One row, so a raise is one statement:
--
--   UPDATE schema_floor SET version = N;
--
-- It starts at this migration's own version: every release before it
-- refuses any newer schema, whatever the floor says.

-- +goose Up

CREATE TABLE schema_floor (
    -- Always true, so the table holds one row.
    singleton boolean NOT NULL DEFAULT true,
    version   bigint  NOT NULL,

    CONSTRAINT schema_floor_pkey            PRIMARY KEY (singleton),
    CONSTRAINT schema_floor_singleton_check CHECK (singleton),
    CONSTRAINT schema_floor_version_check   CHECK (version > 0)
);

INSERT INTO schema_floor (version) VALUES (29);

-- +goose Down

DROP TABLE schema_floor;
