-- name: CreateLink :exec
INSERT INTO links (code, url, visit_count, created_at)
VALUES (?, ?, ?, ?);

-- name: GetLink :one
SELECT code, url, visit_count, created_at
FROM links
WHERE code = ?;

-- name: CountVisit :one
UPDATE links
SET visit_count = visit_count + 1
WHERE code = ?
RETURNING code, url, visit_count, created_at;

-- name: ListLinks :many
SELECT code, url, visit_count, created_at
FROM links
ORDER BY created_at DESC
LIMIT ? OFFSET ?;

-- name: CountLinks :one
SELECT COUNT(*) FROM links;

-- name: DeleteLink :execrows
DELETE FROM links WHERE code = ?;
