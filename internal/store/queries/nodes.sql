-- name: CreateNode :one
INSERT INTO nodes (name, token_hash, settings, created_at) VALUES (?, ?, ?, ?) RETURNING id;

-- name: GetNode :one
SELECT * FROM nodes WHERE id = ?;

-- name: GetNodeByName :one
SELECT * FROM nodes WHERE name = ?;

-- name: GetNodeByToken :one
SELECT * FROM nodes WHERE token_hash = ? AND forgotten = 0;

-- name: ListNodes :many
SELECT * FROM nodes WHERE forgotten = 0 ORDER BY name;

-- name: UpdateNodeSettings :exec
UPDATE nodes SET settings = ? WHERE id = ?;

-- name: UpdateNodeToken :exec
UPDATE nodes SET token_hash = ?, forgotten = 0 WHERE id = ?;

-- name: UpdateNodeSeen :exec
UPDATE nodes SET facts = ?, agent_version = ?, last_seen = ? WHERE id = ?;

-- name: TouchNode :exec
UPDATE nodes SET last_seen = ? WHERE id = ?;

-- name: ForgetNode :exec
UPDATE nodes SET forgotten = 1, token_hash = NULL WHERE id = ?;

-- name: CreateJoinToken :exec
INSERT INTO join_tokens (token_hash, node_name, settings, expires_at, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?);

-- name: GetJoinToken :one
SELECT * FROM join_tokens WHERE token_hash = ?;

-- name: UseJoinToken :execrows
UPDATE join_tokens SET used = 1 WHERE token_hash = ? AND used = 0 AND expires_at >= ?;

-- name: ListJoinTokens :many
SELECT * FROM join_tokens WHERE used = 0 AND expires_at >= ? ORDER BY created_at DESC;

-- name: DeleteJoinToken :exec
DELETE FROM join_tokens WHERE token_hash = ?;

-- name: DeleteOldJoinTokens :exec
DELETE FROM join_tokens WHERE expires_at < ?;
