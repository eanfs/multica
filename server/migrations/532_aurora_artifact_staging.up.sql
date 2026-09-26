-- Aurora artifact staging (Plan C Task 6). One row per object the sandbox
-- uploaded (source_type 'upload') or the server imported from a provider result
-- URL (source_type 'provider_import') before Task 7 commits it as a workspace
-- asset. manifest_artifact_id is the manifest's artifact id for local uploads
-- and NULL for provider imports, whose id is only known once the broker writes
-- the manifest; role and format are likewise NULL for imports because the
-- importer request carries only kind/name/mime.
--
-- The table intentionally declares no primary key and no inline index. The repo
-- convention (see 518-531) builds the id index CONCURRENTLY in its own
-- single-statement migration (533) and attaches it as the primary key (534);
-- the (task_id, manifest_artifact_id) uniqueness index follows in 535. There
-- are no foreign keys or cascading actions: task, generation, and workspace
-- relationships are validated in application code.
CREATE TABLE aurora_artifact_staging (
    id uuid NOT NULL,
    task_id uuid NOT NULL,
    generation_id uuid NOT NULL,
    workspace_id uuid NOT NULL,
    manifest_artifact_id text,
    storage_key text NOT NULL,
    name text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('image', 'video', 'text', 'pdf')),
    role text CHECK (role IN ('primary', 'supporting', 'transcript')),
    format text,
    mime_type text NOT NULL,
    size_bytes bigint NOT NULL CHECK (size_bytes >= 0),
    sha256 text NOT NULL CHECK (sha256 ~ '^sha256:[0-9a-f]{64}$'),
    metadata jsonb NOT NULL DEFAULT '{}',
    source_type text NOT NULL CHECK (source_type IN ('upload', 'provider_import')),
    status text NOT NULL DEFAULT 'staged' CHECK (status IN ('staged', 'committed', 'deleted')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
