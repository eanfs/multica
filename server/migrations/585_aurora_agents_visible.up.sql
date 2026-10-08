-- Aurora skill agents become member-visible like Mika (CreateSystemUserAgent).
-- Preserve system_key and runtime identity for generation lookup. The predicate
-- makes reruns idempotent and leaves ordinary user agents unchanged.
UPDATE agent SET kind = 'user', updated_at = now()
WHERE kind = 'system' AND system_key LIKE 'aurora:%';
