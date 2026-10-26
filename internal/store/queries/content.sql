-- name: CreateKey :exec
INSERT INTO publisher_keys (key_id, public_key, seed_enc, label, is_default, created_at) VALUES (?, ?, ?, ?, ?, ?);

-- name: GetKey :one
SELECT * FROM publisher_keys WHERE key_id = ?;

-- name: GetDefaultKey :one
SELECT * FROM publisher_keys WHERE is_default = 1 LIMIT 1;

-- name: ListKeys :many
SELECT * FROM publisher_keys ORDER BY is_default DESC, created_at;

-- name: DeleteKey :exec
DELETE FROM publisher_keys WHERE key_id = ?;

-- name: ClearDefaultKey :exec
UPDATE publisher_keys SET is_default = 0;

-- name: SetDefaultKey :exec
UPDATE publisher_keys SET is_default = 1 WHERE key_id = ?;

-- name: UpdateKeyLabel :exec
UPDATE publisher_keys SET label = ? WHERE key_id = ?;

-- name: UpdateKeyMe :exec
UPDATE publisher_keys SET me_cache = ?, me_cached_at = ? WHERE key_id = ?;

-- name: CountKeyInstances :one
SELECT count(*) FROM instances WHERE key_id = ?;

-- name: UpsertRelease :exec
INSERT INTO releases (build, image, digest, zip_url, zip_sha256, zip_size, resolved_at, url, notes_url, is_latest, local)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (build) DO UPDATE SET image = excluded.image, digest = excluded.digest,
  zip_url = CASE WHEN excluded.zip_url != '' THEN excluded.zip_url ELSE releases.zip_url END,
  zip_sha256 = CASE WHEN excluded.zip_sha256 != '' THEN excluded.zip_sha256 ELSE releases.zip_sha256 END,
  zip_size = CASE WHEN excluded.zip_size != 0 THEN excluded.zip_size ELSE releases.zip_size END,
  resolved_at = excluded.resolved_at, url = excluded.url, notes_url = excluded.notes_url, local = excluded.local;

-- name: UpdateReleaseZip :exec
UPDATE releases SET zip_url = ?, zip_sha256 = ?, zip_size = ? WHERE build = ?;

-- name: GetRelease :one
SELECT * FROM releases WHERE build = ?;

-- name: GetLatestRelease :one
SELECT * FROM releases WHERE is_latest = 1 LIMIT 1;

-- name: ListReleases :many
SELECT * FROM releases ORDER BY resolved_at DESC;

-- name: ClearLatest :exec
UPDATE releases SET is_latest = 0;

-- name: SetLatest :exec
UPDATE releases SET is_latest = 1 WHERE build = ?;

-- name: DeleteRelease :exec
DELETE FROM releases WHERE build = ?;

-- name: AddLibrary :exec
INSERT INTO library (sha256, kind, name, size, meta, added_by, added_at) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (sha256) DO UPDATE SET kind = excluded.kind, name = excluded.name, meta = excluded.meta;

-- name: GetLibrary :one
SELECT * FROM library WHERE sha256 = ?;

-- name: GetLibraryByName :one
SELECT * FROM library WHERE kind = ? AND name = ?;

-- name: ListLibrary :many
SELECT * FROM library ORDER BY kind, name;

-- name: DeleteLibrary :exec
DELETE FROM library WHERE sha256 = ?;

-- name: DeleteLibraryByName :exec
DELETE FROM library WHERE kind = ? AND name = ?;

-- name: CreateUpload :exec
INSERT INTO uploads (id, kind, name, size, sha256, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetUpload :one
SELECT * FROM uploads WHERE id = ?;

-- name: UpdateUploadReceived :exec
UPDATE uploads SET received = ? WHERE id = ?;

-- name: DeleteUpload :exec
DELETE FROM uploads WHERE id = ?;

-- name: ListOldUploads :many
SELECT * FROM uploads WHERE created_at < ?;
