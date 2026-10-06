-- NULL deliberately means no original proof, including legacy finalized rows.
ALTER TABLE IF EXISTS fleet_namespace_fences ADD COLUMN IF NOT EXISTS completion_manifest jsonb;
