-- One staging row per (task, manifest artifact id) for local uploads, so a
-- replayed upload cannot mint a second object. A NULL manifest_artifact_id
-- (a provider import, whose id the broker assigns after the import returns)
-- never conflicts under a UNIQUE index, so imports stay unconstrained. In its
-- own file because CREATE INDEX CONCURRENTLY cannot share a statement or run
-- inside a transaction, and registered in cmd/migrate/main.go's
-- concurrentIndexCleanups so an interrupted build is not mistaken for success.
CREATE UNIQUE INDEX CONCURRENTLY aurora_artifact_staging_task_artifact_uidx
    ON aurora_artifact_staging (task_id, manifest_artifact_id);
