-- +goose Up
ALTER TABLE plans ADD COLUMN daily_test_generation_limit integer NOT NULL DEFAULT 0
    CONSTRAINT plans_check_daily_test_generation_limit CHECK (daily_test_generation_limit >= 0);
UPDATE plans SET daily_test_generation_limit = 10, updated_at = now() WHERE code = 'pro';
ALTER TABLE test_cases ADD COLUMN is_generated boolean NOT NULL DEFAULT false;

-- Снимок не зависит от последующих правок условия и команд языка.
CREATE TABLE test_generations (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    problem_id uuid NOT NULL REFERENCES problems (id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    request_id uuid NOT NULL,
    task jsonb NOT NULL,
    status integer NOT NULL DEFAULT 1 CONSTRAINT test_generations_check_status CHECK (status BETWEEN 1 AND 4),
    error text NOT NULL DEFAULT '',
    lease_token uuid,
    lease_until timestamptz,
    attempts integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT test_generations_ukey_user_id_request_id UNIQUE (user_id, request_id)
);
CREATE INDEX test_generations_idx_user_id_created_at ON test_generations (user_id, created_at);
CREATE INDEX test_generations_idx_problem_id ON test_generations (problem_id);
CREATE INDEX test_generations_idx_queue ON test_generations (status, lease_until, id) WHERE status IN (1, 2);

CREATE TABLE generated_tests (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    generation_id uuid NOT NULL REFERENCES test_generations (id) ON DELETE CASCADE,
    ordinal integer NOT NULL,
    input bytea NOT NULL,
    expected bytea NOT NULL,
    explanation text NOT NULL,
    rejection_reason text NOT NULL DEFAULT '',
    is_accepted boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT generated_tests_ukey_generation_id_ordinal UNIQUE (generation_id, ordinal),
    CONSTRAINT generated_tests_check_size CHECK (octet_length(input) <= 1048576 AND octet_length(expected) <= 1048576)
);

-- +goose Down
-- Обратные миграции запрещены.
