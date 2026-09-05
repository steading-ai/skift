-- Idempotency keys. See the Patch section of docs/domain-model.md and
-- principle 4 in docs/core-principles.md.
--
-- Every mutation carries a client-supplied request id. A repeat with the
-- same content returns the stored response; the same id with different
-- content is an error rather than a silent replay. request_hash is what
-- tells the two apart: a caller compares the hash of the incoming request
-- with the stored one before replaying the response.

-- +goose Up

CREATE TABLE idempotency_keys (
    org_id       text        NOT NULL,
    request_id   text        NOT NULL,
    -- SHA-256 over the canonical encoding of the request.
    request_hash text        NOT NULL,
    -- The encoded response as first returned.
    response     bytea       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT idempotency_keys_pkey                PRIMARY KEY (org_id, request_id),
    CONSTRAINT idempotency_keys_org_id_fkey         FOREIGN KEY (org_id) REFERENCES organizations (id),
    CONSTRAINT idempotency_keys_request_id_nonempty CHECK (request_id <> ''),
    CONSTRAINT idempotency_keys_request_hash_sha256 CHECK (is_sha256_digest(request_hash))
);

-- Keys are retained for a bounded window, then swept by age.
CREATE INDEX idempotency_keys_created_at_idx ON idempotency_keys (created_at);

-- +goose Down

DROP TABLE idempotency_keys;
