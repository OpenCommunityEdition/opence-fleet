-- name: CreateBan :one
INSERT INTO fleet_bans (hwid, address, discord_id, names, reason, until, created_by, created_at, source_slug)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id;

-- name: GetBan :one
SELECT * FROM fleet_bans WHERE id = ?;

-- name: ListActiveBans :many
SELECT * FROM fleet_bans WHERE removed_at IS NULL AND (until IS NULL OR until > ?) ORDER BY created_at DESC;

-- name: ListBans :many
SELECT * FROM fleet_bans ORDER BY created_at DESC LIMIT ?;

-- name: UpdateBan :exec
UPDATE fleet_bans SET names = ?, reason = ?, until = ? WHERE id = ?;

-- name: RemoveBan :exec
UPDATE fleet_bans SET removed_at = ?, removed_by = ? WHERE id = ?;

-- name: UpsertBanState :exec
INSERT INTO instance_ban_state (ban_id, slug, state, applied_at, line_n, detail) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (ban_id, slug) DO UPDATE SET state = excluded.state, applied_at = excluded.applied_at,
  line_n = excluded.line_n, detail = excluded.detail;

-- name: ListBanStates :many
SELECT * FROM instance_ban_state ORDER BY ban_id, slug;

-- name: ListBanStatesForBan :many
SELECT * FROM instance_ban_state WHERE ban_id = ? ORDER BY slug;

-- name: DeleteBanStates :exec
DELETE FROM instance_ban_state WHERE ban_id = ?;
