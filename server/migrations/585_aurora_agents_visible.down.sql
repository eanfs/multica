-- Reverse only the same 13 historical available keys; preserve all other rows.
-- Restore hidden carriers without changing their identity or archive state.
UPDATE agent SET kind = 'system', updated_at = now()
WHERE kind = 'user' AND system_key IN (
    'aurora:poster',
    'aurora:xhs-image',
    'aurora:product-image',
    'aurora:text-image',
    'aurora:image-edit',
    'aurora:id-photo',
    'aurora:image-video',
    'aurora:text-video',
    'aurora:video-captions',
    'aurora:xhs-copy',
    'aurora:resume',
    'aurora:document-summary',
    'aurora:transcription'
);
