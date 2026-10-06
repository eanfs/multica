-- Aurora workspace nodes carry the workspace/runtime dimensions the Fleet SQL
-- contract did not previously need. Additive and nullable; existing Claude-profile
-- nodes keep NULL and are untouched. No inline index or constraint here.
ALTER TABLE IF EXISTS fleet_nodes ADD COLUMN IF NOT EXISTS workspace_id uuid;
ALTER TABLE IF EXISTS fleet_nodes ADD COLUMN IF NOT EXISTS runtime_id uuid;
