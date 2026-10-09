-- Freeze the 13 available catalog keys at this migration; unavailable placeholders
-- and unknown future keys remain untouched. These agents become visible like Mika.
-- Preserve system_key and runtime identity for generation lookup. The predicate
-- makes reruns idempotent and leaves ordinary user agents unchanged.
UPDATE agent SET kind = 'user', updated_at = now()
WHERE kind = 'system' AND system_key IN (
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
