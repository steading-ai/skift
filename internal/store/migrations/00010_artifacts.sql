-- Blobs, artifacts, and their parts. See the Artifact section of
-- docs/domain-model.md and docs/adrs/003-artifacts-are-content-addressed.md.
--
-- Dagger never stores raw bytes inside a node. A blob row is the identity,
-- media type, and size of bytes held by a storage backend; a part is inline
-- text under the cap, a managed blob, or a reference to something that lives
-- elsewhere. Blobs and references carry a digest.

-- +goose Up

-- The canonical digest form: the algorithm, a colon, and lowercase hex.
-- +goose StatementBegin
CREATE FUNCTION is_sha256_digest(candidate text) RETURNS boolean
    LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
    RETURN candidate ~ '^sha256:[0-9a-f]{64}$';
-- +goose StatementEnd

-- Scoped to an organization, so one tenant cannot learn what another holds
-- by probing digests. A backend is free to deduplicate bytes underneath.
CREATE TABLE blobs (
    org_id     text        NOT NULL,
    digest     text        NOT NULL,
    media_type text        NOT NULL,
    size       bigint      NOT NULL,
    backend    text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT blobs_pkey               PRIMARY KEY (org_id, digest),
    CONSTRAINT blobs_org_id_fkey        FOREIGN KEY (org_id) REFERENCES organizations (id),
    CONSTRAINT blobs_digest_sha256      CHECK (is_sha256_digest(digest)),
    CONSTRAINT blobs_media_type_nonempty CHECK (media_type <> ''),
    CONSTRAINT blobs_size_nonnegative   CHECK (size >= 0),
    CONSTRAINT blobs_backend_nonempty   CHECK (backend <> '')
);

CREATE TABLE artifacts (
    id          text        NOT NULL,
    org_id      text        NOT NULL,
    node_id     text        NOT NULL,
    name        text        NOT NULL,
    description text        NOT NULL DEFAULT '',
    metadata    jsonb       NOT NULL DEFAULT '{}',
    created_by  text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT artifacts_pkey            PRIMARY KEY (id),
    CONSTRAINT artifacts_org_id_id_key   UNIQUE (org_id, id),
    CONSTRAINT artifacts_node_fkey       FOREIGN KEY (org_id, node_id)
        REFERENCES nodes (org_id, id),
    CONSTRAINT artifacts_created_by_fkey FOREIGN KEY (org_id, created_by)
        REFERENCES principals (org_id, id),
    CONSTRAINT artifacts_id_ulid         CHECK (is_ulid(id)),
    CONSTRAINT artifacts_name_nonempty   CHECK (name <> ''),
    CONSTRAINT artifacts_metadata_object CHECK (jsonb_typeof(metadata) = 'object')
);

CREATE INDEX artifacts_node_id_created_at_idx ON artifacts (node_id, created_at);

CREATE TABLE artifact_parts (
    org_id           text   NOT NULL,
    artifact_id      text   NOT NULL,
    position         integer NOT NULL,
    kind             text   NOT NULL,
    -- Exactly one of these three is set, according to kind.
    text             text,
    blob_digest      text,
    reference_uri    text,
    -- The resolved immutable identity behind a reference: a branch is
    -- stored with the commit it pointed at.
    reference_digest text,
    media_type       text,
    size             bigint,

    CONSTRAINT artifact_parts_pkey          PRIMARY KEY (artifact_id, position),
    CONSTRAINT artifact_parts_artifact_fkey FOREIGN KEY (org_id, artifact_id)
        REFERENCES artifacts (org_id, id),
    CONSTRAINT artifact_parts_blob_fkey     FOREIGN KEY (org_id, blob_digest)
        REFERENCES blobs (org_id, digest),
    CONSTRAINT artifact_parts_position_nonnegative CHECK (position >= 0),
    CONSTRAINT artifact_parts_kind_check    CHECK (kind IN ('text', 'blob', 'reference')),
    CONSTRAINT artifact_parts_text_by_kind  CHECK ((kind = 'text') = (text IS NOT NULL)),
    CONSTRAINT artifact_parts_blob_by_kind  CHECK ((kind = 'blob') = (blob_digest IS NOT NULL)),
    CONSTRAINT artifact_parts_reference_by_kind
        CHECK ((kind = 'reference') = (reference_uri IS NOT NULL)
           AND (kind = 'reference') = (reference_digest IS NOT NULL)),
    CONSTRAINT artifact_parts_reference_uri_nonempty CHECK (reference_uri <> ''),
    CONSTRAINT artifact_parts_reference_digest_sha256
        CHECK (reference_digest IS NULL OR is_sha256_digest(reference_digest)),
    CONSTRAINT artifact_parts_media_type_nonempty CHECK (media_type <> ''),
    CONSTRAINT artifact_parts_size_nonnegative    CHECK (size IS NULL OR size >= 0)
);

CREATE INDEX artifact_parts_blob_digest_idx ON artifact_parts (org_id, blob_digest)
    WHERE blob_digest IS NOT NULL;

-- +goose Down

DROP TABLE artifact_parts;
DROP TABLE artifacts;
DROP TABLE blobs;
DROP FUNCTION is_sha256_digest(text);
