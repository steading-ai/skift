-- Full-text search. See the Search section of docs/domain-model.md.
--
-- Lexical search over node titles and bodies, results, and comments, with a
-- GIN index on each. The index expression is a function so a query can
-- name the same expression and be sure to match it.

-- +goose Up

-- +goose StatementBegin
CREATE FUNCTION search_document(content text) RETURNS tsvector
    LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
    RETURN to_tsvector('english', content);
-- +goose StatementEnd

-- A node's document, its title weighted above its body.
-- +goose StatementBegin
CREATE FUNCTION node_search_document(title text, body text) RETURNS tsvector
    LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
    RETURN setweight(search_document(title), 'A') || setweight(search_document(body), 'B');
-- +goose StatementEnd

CREATE INDEX nodes_search_idx    ON nodes    USING gin (node_search_document(title, body));
CREATE INDEX comments_search_idx ON comments USING gin (search_document(body));
CREATE INDEX results_search_idx  ON results  USING gin (search_document(body));

-- +goose Down

DROP INDEX results_search_idx;
DROP INDEX comments_search_idx;
DROP INDEX nodes_search_idx;
DROP FUNCTION node_search_document(text, text);
DROP FUNCTION search_document(text);
