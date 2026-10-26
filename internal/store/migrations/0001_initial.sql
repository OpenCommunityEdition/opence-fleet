-- The first schema (OPENCE_FLEET_PLAN.md 12.5). The schema only grows: a
-- later change is a new file here, applied once, in order.

CREATE TABLE users (
  id INTEGER PRIMARY KEY,
  name TEXT UNIQUE NOT NULL,
  password_hash TEXT NOT NULL,
  role TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  created_by INTEGER,
  disabled INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE sessions (
  token_hash BLOB PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users ON DELETE CASCADE,
  csrf TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  last_used INTEGER NOT NULL,
  address TEXT NOT NULL DEFAULT '',
  remember INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX sessions_user ON sessions (user_id);

CREATE TABLE invites (
  code_hash BLOB PRIMARY KEY,
  role TEXT NOT NULL,
  label TEXT NOT NULL DEFAULT '',
  expires_at INTEGER NOT NULL,
  used_by INTEGER,
  created_by INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE TABLE api_tokens (
  id INTEGER PRIMARY KEY,
  token_hash BLOB UNIQUE NOT NULL,
  label TEXT NOT NULL,
  role TEXT NOT NULL,
  created_by INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  last_used INTEGER
);

CREATE TABLE nodes (
  id INTEGER PRIMARY KEY,
  name TEXT UNIQUE NOT NULL,
  token_hash BLOB UNIQUE,
  settings TEXT NOT NULL DEFAULT '{}',
  facts TEXT NOT NULL DEFAULT '{}',
  agent_version TEXT NOT NULL DEFAULT '',
  last_seen INTEGER,
  created_at INTEGER NOT NULL,
  forgotten INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE join_tokens (
  token_hash BLOB PRIMARY KEY,
  node_name TEXT NOT NULL,
  settings TEXT NOT NULL DEFAULT '{}',
  expires_at INTEGER NOT NULL,
  used INTEGER NOT NULL DEFAULT 0,
  created_by TEXT NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE TABLE publisher_keys (
  key_id TEXT PRIMARY KEY,
  public_key TEXT NOT NULL,
  seed_enc BLOB NOT NULL,
  label TEXT NOT NULL,
  is_default INTEGER NOT NULL DEFAULT 0,
  me_cache TEXT,
  me_cached_at INTEGER,
  created_at INTEGER NOT NULL
);

CREATE TABLE releases (
  build TEXT PRIMARY KEY,
  image TEXT NOT NULL,
  digest TEXT NOT NULL DEFAULT '',
  zip_url TEXT NOT NULL DEFAULT '',
  zip_sha256 TEXT NOT NULL DEFAULT '',
  zip_size INTEGER NOT NULL DEFAULT 0,
  resolved_at INTEGER NOT NULL,
  url TEXT NOT NULL DEFAULT '',
  notes_url TEXT NOT NULL DEFAULT '',
  is_latest INTEGER NOT NULL DEFAULT 0,
  local INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE library (
  sha256 TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  name TEXT NOT NULL,
  size INTEGER NOT NULL,
  meta TEXT NOT NULL DEFAULT '{}',
  added_by TEXT NOT NULL DEFAULT '',
  added_at INTEGER NOT NULL,
  UNIQUE (kind, name)
);

CREATE TABLE uploads (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  name TEXT NOT NULL,
  size INTEGER NOT NULL,
  sha256 TEXT NOT NULL,
  received INTEGER NOT NULL DEFAULT 0,
  created_by TEXT NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE TABLE profiles (
  id INTEGER PRIMARY KEY,
  name TEXT UNIQUE NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  settings TEXT NOT NULL DEFAULT '{}',
  rotation TEXT NOT NULL DEFAULT '',
  region TEXT NOT NULL DEFAULT '',
  init_txt TEXT NOT NULL DEFAULT '',
  channel TEXT NOT NULL DEFAULT 'latest',
  password_policy TEXT NOT NULL DEFAULT 'none',
  password_enc BLOB,
  key_id TEXT REFERENCES publisher_keys ON DELETE SET NULL,
  apply_mode TEXT NOT NULL DEFAULT 'live',
  updated_at INTEGER NOT NULL
);

CREATE TABLE profile_gametypes (
  profile_id INTEGER NOT NULL REFERENCES profiles ON DELETE CASCADE,
  name TEXT NOT NULL,
  toml TEXT NOT NULL,
  PRIMARY KEY (profile_id, name)
);

CREATE TABLE instances (
  slug TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  node_id INTEGER NOT NULL REFERENCES nodes,
  profile_id INTEGER REFERENCES profiles ON DELETE SET NULL,
  overrides TEXT NOT NULL DEFAULT '{}',
  tunnel_port INTEGER NOT NULL,
  web_port INTEGER NOT NULL,
  loopback_index INTEGER NOT NULL,
  web_password_enc BLOB NOT NULL,
  owner_password_enc BLOB,
  password_enc BLOB,
  key_id TEXT REFERENCES publisher_keys ON DELETE SET NULL,
  channel TEXT NOT NULL DEFAULT '',
  running INTEGER NOT NULL DEFAULT 1,
  generation INTEGER NOT NULL DEFAULT 1,
  kind TEXT NOT NULL DEFAULT 'managed',
  external_url TEXT NOT NULL DEFAULT '',
  external_password_enc BLOB,
  notes TEXT NOT NULL DEFAULT '',
  created_by TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  UNIQUE (node_id, tunnel_port),
  UNIQUE (node_id, web_port),
  UNIQUE (node_id, loopback_index)
);
CREATE INDEX instances_node ON instances (node_id);
CREATE INDEX instances_profile ON instances (profile_id);

CREATE TABLE groups (
  name TEXT PRIMARY KEY,
  colour TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT ''
);

CREATE TABLE instance_groups (
  slug TEXT NOT NULL REFERENCES instances ON DELETE CASCADE,
  group_name TEXT NOT NULL REFERENCES groups ON DELETE CASCADE ON UPDATE CASCADE,
  PRIMARY KEY (slug, group_name)
);

CREATE TABLE fleet_bans (
  id INTEGER PRIMARY KEY,
  hwid TEXT NOT NULL DEFAULT '',
  address TEXT NOT NULL DEFAULT '',
  names TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT '',
  until INTEGER,
  created_by TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  source_slug TEXT NOT NULL DEFAULT '',
  removed_at INTEGER,
  removed_by TEXT NOT NULL DEFAULT ''
);

CREATE TABLE instance_ban_state (
  ban_id INTEGER NOT NULL REFERENCES fleet_bans ON DELETE CASCADE,
  slug TEXT NOT NULL REFERENCES instances ON DELETE CASCADE,
  state TEXT NOT NULL DEFAULT 'pending',
  applied_at INTEGER,
  line_n INTEGER,
  detail TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (ban_id, slug)
);

CREATE TABLE events (
  id INTEGER PRIMARY KEY,
  time INTEGER NOT NULL,
  node_id INTEGER,
  slug TEXT NOT NULL DEFAULT '',
  seq INTEGER NOT NULL DEFAULT 0,
  type TEXT NOT NULL,
  data TEXT NOT NULL
);
CREATE INDEX events_slug_time ON events (slug, time);
CREATE INDEX events_time ON events (time);

CREATE TABLE games (
  id INTEGER PRIMARY KEY,
  slug TEXT NOT NULL,
  started INTEGER NOT NULL,
  ended INTEGER,
  map TEXT NOT NULL DEFAULT '',
  gametype TEXT NOT NULL DEFAULT '',
  variant TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT '',
  duration INTEGER NOT NULL DEFAULT 0,
  peak_players INTEGER NOT NULL DEFAULT 0,
  team_scores TEXT NOT NULL DEFAULT '',
  players TEXT NOT NULL DEFAULT ''
);
CREATE INDEX games_slug_started ON games (slug, started);

CREATE TABLE instance_samples (
  slug TEXT NOT NULL,
  time INTEGER NOT NULL,
  players INTEGER NOT NULL,
  frame_ms_max REAL NOT NULL DEFAULT 0,
  PRIMARY KEY (slug, time)
);
CREATE INDEX instance_samples_time ON instance_samples (time);

CREATE TABLE jobs (
  id INTEGER PRIMARY KEY,
  kind TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  params TEXT NOT NULL DEFAULT '{}',
  state TEXT NOT NULL,
  actor TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  finished_at INTEGER,
  detail TEXT NOT NULL DEFAULT ''
);

CREATE TABLE job_items (
  job_id INTEGER NOT NULL REFERENCES jobs ON DELETE CASCADE,
  slug TEXT NOT NULL,
  wave INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (job_id, slug)
);

CREATE TABLE schedules (
  id INTEGER PRIMARY KEY,
  name TEXT UNIQUE NOT NULL,
  cron TEXT NOT NULL,
  action TEXT NOT NULL,
  targets TEXT NOT NULL,
  params TEXT NOT NULL DEFAULT '{}',
  enabled INTEGER NOT NULL DEFAULT 1,
  last_run INTEGER,
  next_run INTEGER,
  last_job INTEGER
);

CREATE TABLE webhooks (
  id INTEGER PRIMARY KEY,
  label TEXT NOT NULL,
  url_enc BLOB NOT NULL,
  kind TEXT NOT NULL,
  rules TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  last_status TEXT NOT NULL DEFAULT ''
);

CREATE TABLE alerts (
  id INTEGER PRIMARY KEY,
  rule TEXT NOT NULL,
  target TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  first_seen INTEGER NOT NULL,
  last_seen INTEGER NOT NULL,
  resolved_at INTEGER,
  acknowledged_by TEXT NOT NULL DEFAULT '',
  delivered TEXT NOT NULL DEFAULT ''
);
CREATE INDEX alerts_open ON alerts (resolved_at, rule, target);

CREATE TABLE audit (
  id INTEGER PRIMARY KEY,
  time INTEGER NOT NULL,
  actor TEXT NOT NULL,
  action TEXT NOT NULL,
  target TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX audit_time ON audit (time);

CREATE TABLE settings (
  name TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
