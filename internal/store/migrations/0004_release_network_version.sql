-- The game's network version of each release (0: not known yet), and where
-- it was read: label (the image's org.opence.network-version), notes (the
-- GitHub release's "Network version: N" line) or server (a server that runs
-- the build said it).
ALTER TABLE releases ADD COLUMN network_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE releases ADD COLUMN network_source TEXT NOT NULL DEFAULT '';
