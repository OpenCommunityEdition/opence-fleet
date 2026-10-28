-- A fleet ban of a verified Discord account (the game's link capability):
-- its id, or '' for a ban by address or hardware id alone.
ALTER TABLE fleet_bans ADD COLUMN discord_id TEXT NOT NULL DEFAULT '';
