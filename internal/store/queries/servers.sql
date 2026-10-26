-- name: CreateProfile :one
INSERT INTO profiles (name, description, settings, rotation, region, init_txt, channel, password_policy, password_enc,
  key_id, apply_mode, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id;

-- name: GetProfile :one
SELECT * FROM profiles WHERE id = ?;

-- name: GetProfileByName :one
SELECT * FROM profiles WHERE name = ?;

-- name: ListProfiles :many
SELECT * FROM profiles ORDER BY name;

-- name: UpdateProfile :exec
UPDATE profiles SET name = ?, description = ?, settings = ?, rotation = ?, region = ?, init_txt = ?, channel = ?,
  password_policy = ?, password_enc = ?, key_id = ?, apply_mode = ?, updated_at = ?
WHERE id = ?;

-- name: DeleteProfile :exec
DELETE FROM profiles WHERE id = ?;

-- name: CountProfileFollowers :one
SELECT count(*) FROM instances WHERE profile_id = ?;

-- name: ListProfileGametypes :many
SELECT * FROM profile_gametypes WHERE profile_id = ? ORDER BY name;

-- name: UpsertProfileGametype :exec
INSERT INTO profile_gametypes (profile_id, name, toml) VALUES (?, ?, ?)
ON CONFLICT (profile_id, name) DO UPDATE SET toml = excluded.toml;

-- name: DeleteProfileGametype :exec
DELETE FROM profile_gametypes WHERE profile_id = ? AND name = ?;

-- name: CreateInstance :exec
INSERT INTO instances (slug, name, node_id, profile_id, overrides, tunnel_port, web_port, loopback_index,
  web_password_enc, owner_password_enc, password_enc, key_id, channel, running, kind, external_url, external_password_enc,
  notes, created_by, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetInstance :one
SELECT * FROM instances WHERE slug = ?;

-- name: ListInstances :many
SELECT * FROM instances ORDER BY slug;

-- name: ListInstancesByNode :many
SELECT * FROM instances WHERE node_id = ? ORDER BY slug;

-- name: ListInstancesByProfile :many
SELECT * FROM instances WHERE profile_id = ? ORDER BY slug;

-- name: UpdateInstance :exec
UPDATE instances SET name = ?, profile_id = ?, overrides = ?, key_id = ?, channel = ?, notes = ?,
  generation = generation + 1
WHERE slug = ?;

-- name: UpdateInstancePorts :exec
UPDATE instances SET node_id = ?, tunnel_port = ?, web_port = ?, loopback_index = ?, generation = generation + 1
WHERE slug = ?;

-- name: SetInstanceRunning :exec
UPDATE instances SET running = ?, generation = generation + 1 WHERE slug = ?;

-- name: BumpInstanceGeneration :exec
UPDATE instances SET generation = generation + 1 WHERE slug = ?;

-- name: BumpProfileGenerations :exec
UPDATE instances SET generation = generation + 1 WHERE profile_id = ?;

-- name: SetInstanceWebPassword :exec
UPDATE instances SET web_password_enc = ?, generation = generation + 1 WHERE slug = ?;

-- name: SetInstancePassword :exec
UPDATE instances SET password_enc = ?, generation = generation + 1 WHERE slug = ?;

-- name: DeleteInstance :exec
DELETE FROM instances WHERE slug = ?;

-- name: CountInstances :one
SELECT count(*) FROM instances;

-- name: UpsertGroup :exec
INSERT INTO groups (name, colour, description) VALUES (?, ?, ?)
ON CONFLICT (name) DO UPDATE SET colour = excluded.colour, description = excluded.description;

-- name: ListGroups :many
SELECT * FROM groups ORDER BY name;

-- name: DeleteGroup :exec
DELETE FROM groups WHERE name = ?;

-- name: AddInstanceGroup :exec
INSERT OR IGNORE INTO instance_groups (slug, group_name) VALUES (?, ?);

-- name: RemoveInstanceGroup :exec
DELETE FROM instance_groups WHERE slug = ? AND group_name = ?;

-- name: ListInstanceGroups :many
SELECT * FROM instance_groups ORDER BY slug, group_name;

-- name: ListGroupMembers :many
SELECT slug FROM instance_groups WHERE group_name = ? ORDER BY slug;
