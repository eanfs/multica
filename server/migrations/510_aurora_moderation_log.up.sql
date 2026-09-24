-- Aurora moderation audit trail: one row per piece of content the moderator
-- rejected, so a rejection is reviewable after the fact (Plan safety Task 2).
--
-- generation_id is NULL for a prompt rejected before its generation row exists
-- — the screen runs ahead of the insert, which is what keeps rejected prompts
-- from leaving queued work behind. There is no foreign key by house rule, so
-- the association is application-level and a generation deleted later leaves
-- its audit rows intact (the log is evidence, not a dependent).
--
-- scope names the surface screened (prompt | asset); verdict names the outcome
-- (blocked | allowed). The default adapter writes "blocked" only: an audit row
-- per admitted request would be a write on the hot path with nothing to review.
-- "allowed" stays in the domain for an adapter that records every verdict.
CREATE TABLE IF NOT EXISTS aurora_moderation_log (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    generation_id UUID,
    workspace_id UUID NOT NULL,
    scope TEXT NOT NULL,
    verdict TEXT NOT NULL,
    reason TEXT NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
