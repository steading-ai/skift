-- Organizations are the tenancy root. Every other table carries an org_id,
-- and every query is scoped by it: see principle 8 in docs/core-principles.md.

-- +goose Up

-- Identifiers are ULIDs held as their canonical 26-character Crockford
-- base32 text. The regex is the whole grammar: the first character carries
-- the two spare high bits, so it never exceeds 7.
-- +goose StatementBegin
CREATE FUNCTION is_ulid(candidate text) RETURNS boolean
    LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
    RETURN candidate ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$';
-- +goose StatementEnd

CREATE TABLE organizations (
    id         text        NOT NULL,
    name       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT organizations_pkey    PRIMARY KEY (id),
    CONSTRAINT organizations_id_ulid CHECK (is_ulid(id)),
    CONSTRAINT organizations_name_nonempty CHECK (name <> '')
);

-- +goose Down

DROP TABLE organizations;
DROP FUNCTION is_ulid(text);
