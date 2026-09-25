-- Signup bonus eligibility is explicit so pre-existing users are never backfilled.
CREATE TABLE IF NOT EXISTS aurora_signup_bonus (
    user_id UUID NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'granted')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
