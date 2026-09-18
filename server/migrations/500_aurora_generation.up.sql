-- Aurora content-creation generation: one row per user submission, keyed to an
-- agent_task_queue row once execution is wired (Plan 3). No foreign key by house
-- rule; workspace membership is re-validated in application code on every write.
CREATE TABLE IF NOT EXISTS aurora_generation (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    user_id UUID NOT NULL,
    skill_id TEXT NOT NULL,
    prompt TEXT NOT NULL,
    status TEXT NOT NULL,
    task_id UUID,
    credits_reserved BIGINT NOT NULL DEFAULT 0,
    credits_charged BIGINT NOT NULL DEFAULT 0,
    error TEXT,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
