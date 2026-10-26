-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: CreateUser :one
INSERT INTO users (name, password_hash, role, created_at, created_by)
VALUES (?, ?, ?, ?, ?) RETURNING id;

-- name: GetUser :one
SELECT * FROM users WHERE id = ?;

-- name: GetUserByName :one
SELECT * FROM users WHERE name = ? COLLATE NOCASE;

-- name: ListUsers :many
SELECT * FROM users ORDER BY name;

-- name: UpdateUserRole :exec
UPDATE users SET role = ? WHERE id = ?;

-- name: UpdateUserPassword :exec
UPDATE users SET password_hash = ? WHERE id = ?;

-- name: SetUserDisabled :exec
UPDATE users SET disabled = ? WHERE id = ?;

-- name: CountOwners :one
SELECT count(*) FROM users WHERE role = 'owner' AND disabled = 0;

-- name: CreateSession :exec
INSERT INTO sessions (token_hash, user_id, csrf, created_at, expires_at, last_used, address, remember)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetSession :one
SELECT sessions.token_hash, sessions.user_id, sessions.csrf, sessions.created_at, sessions.expires_at,
  sessions.last_used, sessions.address, sessions.remember, users.name, users.role, users.disabled
FROM sessions JOIN users ON users.id = sessions.user_id
WHERE sessions.token_hash = ?;

-- name: TouchSession :exec
UPDATE sessions SET last_used = ? WHERE token_hash = ?;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE token_hash = ?;

-- name: DeleteUserSessions :exec
DELETE FROM sessions WHERE user_id = ?;

-- name: ListSessions :many
SELECT sessions.token_hash, sessions.user_id, sessions.created_at, sessions.expires_at, sessions.last_used,
  sessions.address, sessions.remember, users.name
FROM sessions JOIN users ON users.id = sessions.user_id
ORDER BY sessions.last_used DESC;

-- name: DeleteExpiredSessions :exec
DELETE FROM sessions WHERE expires_at < ?;

-- name: CreateInvite :exec
INSERT INTO invites (code_hash, role, label, expires_at, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?);

-- name: GetInvite :one
SELECT * FROM invites WHERE code_hash = ?;

-- name: UseInvite :execrows
UPDATE invites SET used_by = ? WHERE code_hash = ? AND used_by IS NULL AND expires_at >= ?;

-- name: ListInvites :many
SELECT * FROM invites WHERE used_by IS NULL AND expires_at >= ? ORDER BY created_at DESC;

-- name: DeleteInvite :exec
DELETE FROM invites WHERE code_hash = ?;

-- name: CreateToken :one
INSERT INTO api_tokens (token_hash, label, role, created_by, created_at) VALUES (?, ?, ?, ?, ?) RETURNING id;

-- name: GetTokenByHash :one
SELECT api_tokens.*, users.name AS user_name, users.disabled AS user_disabled
FROM api_tokens JOIN users ON users.id = api_tokens.created_by
WHERE api_tokens.token_hash = ?;

-- name: TouchToken :exec
UPDATE api_tokens SET last_used = ? WHERE id = ?;

-- name: ListTokens :many
SELECT api_tokens.*, users.name AS user_name FROM api_tokens JOIN users ON users.id = api_tokens.created_by
ORDER BY api_tokens.created_at DESC;

-- name: DeleteToken :exec
DELETE FROM api_tokens WHERE id = ?;
