-- One eligibility state per user.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS aurora_signup_bonus_user_idx
    ON aurora_signup_bonus (user_id);
