-- +goose Up
-- Флаг уже присутствует в ранних установках; старые задания остаются с прежним выбором автора.
ALTER TABLE problems ADD COLUMN IF NOT EXISTS allow_ai_hints boolean NOT NULL DEFAULT false;
ALTER TABLE plans ADD COLUMN daily_ai_hint_limit integer NOT NULL DEFAULT 0
    CONSTRAINT plans_check_daily_ai_hint_limit CHECK (daily_ai_hint_limit >= 0);
UPDATE plans SET daily_ai_hint_limit = 20, updated_at = now() WHERE code = 'pro';

-- Резерв до внешнего вызова защищает квоту и идемпотентность между экземплярами API.
CREATE TABLE submission_hints (
    id bigint GENERATED ALWAYS AS IDENTITY CONSTRAINT submission_hints_pkey PRIMARY KEY,
    submission_id uuid NOT NULL CONSTRAINT submission_hints_fkey_submission_id REFERENCES submissions (id) ON DELETE CASCADE,
    user_id uuid NOT NULL CONSTRAINT submission_hints_fkey_user_id REFERENCES users (id) ON DELETE CASCADE,
    text text NOT NULL DEFAULT '',
    is_fallback boolean NOT NULL DEFAULT false,
    lease_token uuid,
    lease_until timestamptz,
    reserved_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT submission_hints_ukey_submission_id UNIQUE (submission_id),
    CONSTRAINT submission_hints_check_text CHECK (char_length(text) <= 1000),
    CONSTRAINT submission_hints_check_state CHECK (
        (text = '' AND lease_token IS NOT NULL AND lease_until IS NOT NULL) OR
        (text <> '' AND lease_token IS NULL AND lease_until IS NULL))
);
CREATE INDEX submission_hints_idx_user_id_reserved_at ON submission_hints (user_id, reserved_at);

-- +goose Down
-- Обратные миграции запрещены.
