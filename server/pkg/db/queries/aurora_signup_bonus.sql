-- name: CreateAuroraSignupBonusEligibility :exec
INSERT INTO aurora_signup_bonus (user_id, status)
VALUES ($1, 'pending')
ON CONFLICT (user_id) DO NOTHING;

-- name: GetAuroraSignupBonusStatus :one
SELECT status FROM aurora_signup_bonus WHERE user_id = $1;

-- name: MarkAuroraSignupBonusGranted :exec
UPDATE aurora_signup_bonus
SET status = 'granted', updated_at = now()
WHERE user_id = $1 AND status = 'pending';
