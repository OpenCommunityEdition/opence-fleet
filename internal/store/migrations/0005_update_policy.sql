-- The one-off of this version, for a fleet that ran before it (one with a
-- user or a node; a new fleet has these as its defaults): the game servers'
-- update policy set to install and the release check to every 30 minutes,
-- whatever they were, and the change marked for the first start to tell
-- the owners (update_policy_migrated: pending, then the version that told
-- them). A migration runs once in a database's life (schema_version), so
-- a later upgrade, or a value an admin sets afterwards, is never touched.
INSERT INTO settings (name, value)
SELECT 'server_update_mode', 'install' WHERE EXISTS (SELECT 1 FROM users) OR EXISTS (SELECT 1 FROM nodes)
ON CONFLICT (name) DO UPDATE SET value = excluded.value;
INSERT INTO settings (name, value)
SELECT 'release_check_interval', '30m' WHERE EXISTS (SELECT 1 FROM users) OR EXISTS (SELECT 1 FROM nodes)
ON CONFLICT (name) DO UPDATE SET value = excluded.value;
INSERT INTO settings (name, value)
SELECT 'update_policy_migrated', 'pending' WHERE EXISTS (SELECT 1 FROM users) OR EXISTS (SELECT 1 FROM nodes)
ON CONFLICT (name) DO UPDATE SET value = excluded.value;
