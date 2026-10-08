-- Restore hidden carriers without changing their identity or archive state.
UPDATE agent SET kind = 'system', updated_at = now()
WHERE kind = 'user' AND system_key LIKE 'aurora:%';
