-- Comments and their revisions. See the Comment section of
-- docs/domain-model.md.
--
-- A comment is content: the one thing here that is edited, threaded, and
-- resolved, which is why it is its own entity and not a kind of event.
-- Editing keeps the prior body: a trigger moves it to comment_revisions,
-- so no path through the schema can lose one.

-- +goose Up

CREATE TABLE comments (
    id          text        NOT NULL,
    org_id      text        NOT NULL,
    node_id     text        NOT NULL,
    author      text        NOT NULL,
    body        text        NOT NULL,
    -- Threading. A reply is on the same node as its parent.
    in_reply_to text,
    resolved_at timestamptz,
    resolved_by text,
    created_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT comments_pkey              PRIMARY KEY (id),
    CONSTRAINT comments_node_id_id_key    UNIQUE (node_id, id),
    CONSTRAINT comments_node_fkey         FOREIGN KEY (org_id, node_id)
        REFERENCES nodes (org_id, id),
    CONSTRAINT comments_author_fkey       FOREIGN KEY (org_id, author)
        REFERENCES principals (org_id, id),
    CONSTRAINT comments_in_reply_to_fkey  FOREIGN KEY (node_id, in_reply_to)
        REFERENCES comments (node_id, id),
    CONSTRAINT comments_resolved_by_fkey  FOREIGN KEY (org_id, resolved_by)
        REFERENCES principals (org_id, id),
    CONSTRAINT comments_id_ulid           CHECK (is_ulid(id)),
    CONSTRAINT comments_body_nonempty     CHECK (body <> ''),
    CONSTRAINT comments_not_own_reply     CHECK (in_reply_to <> id),
    -- Resolution records who and when together, or not at all.
    CONSTRAINT comments_resolved_together CHECK ((resolved_at IS NULL) = (resolved_by IS NULL))
);

-- A node's comments in order, and a thread's replies.
CREATE INDEX comments_node_id_created_at_idx ON comments (node_id, created_at);
CREATE INDEX comments_in_reply_to_idx ON comments (in_reply_to)
    WHERE in_reply_to IS NOT NULL;

-- Prior bodies. revision counts up from 1 in edit order, and created_at is
-- when the body was replaced.
CREATE TABLE comment_revisions (
    comment_id text        NOT NULL,
    revision   integer     NOT NULL,
    body       text        NOT NULL,
    author     text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT comment_revisions_pkey              PRIMARY KEY (comment_id, revision),
    CONSTRAINT comment_revisions_comment_fkey      FOREIGN KEY (comment_id)
        REFERENCES comments (id),
    CONSTRAINT comment_revisions_author_fkey       FOREIGN KEY (author)
        REFERENCES principals (id),
    CONSTRAINT comment_revisions_revision_positive CHECK (revision >= 1)
);

-- +goose StatementBegin
CREATE FUNCTION comments_keep_revision() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.body IS DISTINCT FROM OLD.body THEN
        INSERT INTO comment_revisions (comment_id, revision, body, author)
        SELECT OLD.id, coalesce(max(revision), 0) + 1, OLD.body, OLD.author
          FROM comment_revisions
         WHERE comment_id = OLD.id;
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER comments_keep_revision
    BEFORE UPDATE OF body ON comments
    FOR EACH ROW EXECUTE FUNCTION comments_keep_revision();

CREATE TRIGGER comment_revisions_append_only
    BEFORE UPDATE OR DELETE ON comment_revisions
    FOR EACH ROW EXECUTE FUNCTION raise_append_only();

CREATE TRIGGER comment_revisions_no_truncate
    BEFORE TRUNCATE ON comment_revisions
    FOR EACH STATEMENT EXECUTE FUNCTION raise_append_only();

-- +goose Down

DROP TABLE comment_revisions;
DROP TABLE comments;
DROP FUNCTION comments_keep_revision();
