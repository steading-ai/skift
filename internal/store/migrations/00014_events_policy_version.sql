-- Every event results from an authorized mutation, and records the version
-- of the policy bundle that authorized it, so an audit can say which rules
-- were in force. See docs/adrs/004-embedded-opa-authorization.md.

-- +goose Up

ALTER TABLE events
    ADD COLUMN policy_version text NOT NULL,
    ADD CONSTRAINT events_policy_version_nonempty CHECK (policy_version <> '');

-- +goose Down

ALTER TABLE events
    DROP CONSTRAINT events_policy_version_nonempty,
    DROP COLUMN policy_version;
