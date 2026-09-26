ALTER TABLE aurora_asset
    DROP COLUMN IF EXISTS manifest_artifact_id,
    DROP COLUMN IF EXISTS name,
    DROP COLUMN IF EXISTS mime_type,
    DROP COLUMN IF EXISTS size_bytes,
    DROP COLUMN IF EXISTS sha256,
    DROP COLUMN IF EXISTS role,
    DROP COLUMN IF EXISTS metadata;
