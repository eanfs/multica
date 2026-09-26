-- Asset idempotency and metadata (Plan C Task 6). Task 7 reports artifacts by
-- staging id and must write at most one aurora_asset per manifest artifact, so
-- the manifest artifact id is recorded beside the generation. The remaining
-- columns carry validated manifest metadata through to the library. All are
-- nullable because the pre-existing Plan 3 writeback rows have no manifest.
ALTER TABLE aurora_asset
    ADD COLUMN IF NOT EXISTS manifest_artifact_id text,
    ADD COLUMN IF NOT EXISTS name text,
    ADD COLUMN IF NOT EXISTS mime_type text,
    ADD COLUMN IF NOT EXISTS size_bytes bigint,
    ADD COLUMN IF NOT EXISTS sha256 text,
    ADD COLUMN IF NOT EXISTS role text,
    ADD COLUMN IF NOT EXISTS metadata jsonb NOT NULL DEFAULT '{}';
