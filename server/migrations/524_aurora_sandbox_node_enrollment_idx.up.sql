-- A live enrollment-token hash is unique; cleared rows (NULL) are exempt so
-- many stopped nodes do not collide. In its own file because CREATE INDEX
-- CONCURRENTLY cannot share a statement or run inside a transaction, and
-- registered in cmd/migrate/main.go's concurrentIndexCleanups so an interrupted
-- build is not mistaken for success on retry.
CREATE UNIQUE INDEX CONCURRENTLY aurora_sandbox_node_enrollment_uidx ON aurora_sandbox_node (enrollment_token_hash) WHERE enrollment_token_hash IS NOT NULL;
