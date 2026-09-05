-- Lexical search. See the Search section of docs/domain-model.md and
-- migration 00012_search.sql, which defines the document functions the
-- predicates below name so that each arm is served by its GIN index:
-- nodes_search_idx, results_search_idx, and comments_search_idx.
--
-- A node is a hit when its title or body, any of its results, or any of
-- its comments matches. Its rank is the sum over its matching sources,
-- and its snippet comes from the source that matched best. Order is by
-- rank and then id, so two runs over the same rows return the same page.
--
-- Archived projects are hidden unless asked for, and a project filter is
-- optional; the organization predicate is static on every table touched.
-- name: SearchNodes :many
WITH hits AS (
    SELECT n.id AS node_id, 'node'::text AS source,
           ts_rank_cd(node_search_document(n.title, n.body), websearch_to_tsquery('english', sqlc.arg(query))) AS rank,
           n.title || E'\n\n' || n.body AS text
    FROM nodes n
    WHERE n.org_id = $1
      AND node_search_document(n.title, n.body) @@ websearch_to_tsquery('english', sqlc.arg(query))
    UNION ALL
    SELECT r.node_id, 'result'::text,
           ts_rank_cd(search_document(r.body), websearch_to_tsquery('english', sqlc.arg(query))),
           r.body
    FROM results r
    WHERE r.org_id = $1
      AND search_document(r.body) @@ websearch_to_tsquery('english', sqlc.arg(query))
    UNION ALL
    SELECT c.node_id, 'comment'::text,
           ts_rank_cd(search_document(c.body), websearch_to_tsquery('english', sqlc.arg(query))),
           c.body
    FROM comments c
    WHERE c.org_id = $1
      AND search_document(c.body) @@ websearch_to_tsquery('english', sqlc.arg(query))
), ranked AS (
    SELECT node_id, source, text,
           sum(rank) OVER (PARTITION BY node_id) AS rank,
           row_number() OVER (PARTITION BY node_id ORDER BY rank DESC, source) AS position
    FROM hits
)
SELECT sqlc.embed(n),
       h.rank::real AS rank,
       h.source::text AS source,
       ts_headline('english', h.text, websearch_to_tsquery('english', sqlc.arg(query)),
                   'MaxFragments=2, MaxWords=24, MinWords=12')::text AS snippet
FROM ranked h
JOIN nodes n ON n.org_id = $1 AND n.id = h.node_id
JOIN projects p ON p.org_id = $1 AND p.id = n.project_id
WHERE h.position = 1
  AND (p.archived_at IS NULL OR sqlc.arg(include_archived)::boolean)
  AND (sqlc.narg(project_id)::text IS NULL OR n.project_id = sqlc.narg(project_id)::text)
ORDER BY h.rank DESC, n.id
LIMIT sqlc.arg(page_size) OFFSET sqlc.arg(page_offset);
