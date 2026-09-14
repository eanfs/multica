-- Content asset produced by a generation (image/video/text/document). Linked to
-- its generation by application-level id; no FK by house rule.
CREATE TABLE IF NOT EXISTS aurora_asset (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    generation_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    kind TEXT NOT NULL,
    media_url TEXT,
    format TEXT,
    created_at timestamptz NOT NULL DEFAULT now()
);
