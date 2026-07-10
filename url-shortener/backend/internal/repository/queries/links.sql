-- name: CreateLink :exec
INSERT INTO links (code, url, created_at)
VALUES (?, ?, ?);

-- name: GetLinkURL :one
SELECT url FROM links WHERE code = ?;

-- name: GetLinkMeta :one
SELECT code, url, created_at FROM links WHERE code = ?;

-- name: ListLinks :many
SELECT l.code, l.url, l.created_at,
       (SELECT COUNT(*) FROM link_events e WHERE e.code = l.code) AS visit_count
FROM links l
ORDER BY l.created_at DESC
LIMIT ? OFFSET ?;

-- name: CountLinks :one
SELECT COUNT(*) FROM links;

-- name: DeleteLink :execrows
DELETE FROM links WHERE code = ?;

-- name: InsertEvent :exec
INSERT INTO link_events (code, accessed_at, referer, user_agent)
VALUES (?, ?, ?, ?);

-- name: ListEvents :many
SELECT id, code, accessed_at, referer, user_agent
FROM link_events
WHERE code = ?
ORDER BY accessed_at DESC
LIMIT ? OFFSET ?;

-- name: CountEvents :one
SELECT COUNT(*) FROM link_events WHERE code = ?;
