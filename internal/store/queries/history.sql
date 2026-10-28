-- name: InsertEvent :exec
INSERT INTO events (time, node_id, slug, seq, type, data) VALUES (?, ?, ?, ?, ?, ?);

-- name: ListEventsBySlug :many
SELECT * FROM events WHERE slug = ? AND id < ? ORDER BY id DESC LIMIT ?;

-- name: ListRecentEvents :many
SELECT * FROM events WHERE type IN (sqlc.slice('types')) ORDER BY id DESC LIMIT ?;

-- name: ListEventsSince :many
SELECT * FROM events WHERE id > ? ORDER BY id LIMIT ?;

-- name: PruneEvents :exec
DELETE FROM events WHERE time < ?;

-- name: StartGame :one
INSERT INTO games (slug, started, map, gametype, variant, peak_players) VALUES (?, ?, ?, ?, ?, ?) RETURNING id;

-- name: OpenGame :one
SELECT * FROM games WHERE slug = ? AND ended IS NULL ORDER BY started DESC LIMIT 1;

-- name: EndGame :exec
UPDATE games SET ended = ?, reason = ?, duration = ?, team_scores = ?, players = ?,
  peak_players = max(peak_players, CAST(sqlc.arg(peak) AS INTEGER)) WHERE id = sqlc.arg(id);

-- name: UpdateGamePeak :exec
UPDATE games SET peak_players = max(peak_players, CAST(sqlc.arg(peak) AS INTEGER)) WHERE id = sqlc.arg(id);

-- name: CloseOpenGames :exec
UPDATE games SET ended = ?, reason = 'unknown' WHERE slug = ? AND ended IS NULL;

-- name: ListGames :many
SELECT * FROM games WHERE slug = ? ORDER BY started DESC LIMIT ?;

-- name: CountGames :one
SELECT count(*) FROM games WHERE slug = ? AND ended IS NOT NULL;

-- name: PruneGames :exec
DELETE FROM games WHERE started < ?;

-- name: InsertSample :exec
INSERT OR REPLACE INTO instance_samples (slug, time, players, frame_ms_max) VALUES (?, ?, ?, ?);

-- name: ListSamplesSince :many
SELECT * FROM instance_samples WHERE time >= ? ORDER BY time;

-- name: ListInstanceSamples :many
SELECT * FROM instance_samples WHERE slug = ? AND time >= ? ORDER BY time;

-- name: PruneSamples :exec
DELETE FROM instance_samples WHERE time < ?;

-- name: CountEventsSince :one
SELECT count(*) FROM events WHERE slug = ? AND type = ? AND time >= ?;
