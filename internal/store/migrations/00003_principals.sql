-- Principals and the identities that reach them. See the Principal section of
-- docs/domain-model.md and the trust boundary in docs/architecture.md.
--
-- A principal is the stable id events and leases refer to. An identity is
-- how a transport recognised the caller, and one principal holds as many as
-- it has transports. Schemes are open: adding one is inserting a row, so
-- nothing here enumerates them.

-- +goose Up

CREATE TABLE principals (
    id         text        NOT NULL,
    org_id     text        NOT NULL,
    kind       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT principals_pkey          PRIMARY KEY (id),
    -- As on projects: lets child tables reference (org_id, id) together.
    CONSTRAINT principals_org_id_id_key UNIQUE (org_id, id),
    CONSTRAINT principals_org_id_fkey   FOREIGN KEY (org_id) REFERENCES organizations (id),
    CONSTRAINT principals_id_ulid       CHECK (is_ulid(id)),
    CONSTRAINT principals_kind_check    CHECK (kind IN ('user', 'agent', 'service'))
);

CREATE TABLE principal_identities (
    principal_id text        NOT NULL,
    scheme       text        NOT NULL,
    value        text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT principal_identities_pkey           PRIMARY KEY (scheme, value),
    CONSTRAINT principal_identities_principal_fkey FOREIGN KEY (principal_id)
        REFERENCES principals (id) ON DELETE CASCADE,
    CONSTRAINT principal_identities_scheme_nonempty CHECK (scheme <> ''),
    CONSTRAINT principal_identities_value_nonempty  CHECK (value <> '')
);

CREATE INDEX principal_identities_principal_id_idx ON principal_identities (principal_id);

-- +goose Down

DROP TABLE principal_identities;
DROP TABLE principals;
