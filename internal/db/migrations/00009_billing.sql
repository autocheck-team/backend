-- +goose Up
CREATE TABLE plans (
    code text PRIMARY KEY,
    name text NOT NULL,
    amount_kopecks bigint NOT NULL CHECK (amount_kopecks >= 0),
    period_days integer NOT NULL CHECK (period_days > 0),
    compilation_limit integer NOT NULL CHECK (compilation_limit >= 0),
    compilation_window_seconds integer NOT NULL CHECK (compilation_window_seconds > 0),
    monthly_test_run_limit integer NOT NULL CHECK (monthly_test_run_limit >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO plans (code, name, amount_kopecks, period_days, compilation_limit,
                   compilation_window_seconds, monthly_test_run_limit) VALUES
    ('free', 'Бесплатный', 0, 30, 1, 60, 100),
    ('pro', 'Pro', 29900, 30, 0, 60, 0);

CREATE TABLE subscriptions (
    user_id uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    plan_code text NOT NULL REFERENCES plans (code),
    status text NOT NULL CHECK (status IN ('active', 'canceled')),
    starts_at timestamptz NOT NULL,
    ends_at timestamptz NOT NULL CHECK (ends_at > starts_at),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Снимок цены и длительности сохраняет условия покупки при изменении каталога.
CREATE TABLE payments (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    plan_code text NOT NULL REFERENCES plans (code),
    request_id uuid NOT NULL,
    provider_id text UNIQUE,
    amount_kopecks bigint NOT NULL CHECK (amount_kopecks > 0),
    period_days integer NOT NULL CHECK (period_days > 0),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'succeeded', 'canceled')),
    idempotence_key uuid NOT NULL UNIQUE,
    return_url text NOT NULL,
    confirmation_url text NOT NULL DEFAULT '',
    next_sync_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, request_id)
);
CREATE INDEX payments_user_idx ON payments (user_id, id DESC);
CREATE INDEX payments_pending_idx ON payments (next_sync_at, id) WHERE status = 'pending';

-- Дата прогона нужна для календарного месяца, дата посылки может быть из прошлого месяца.
ALTER TABLE test_runs ADD COLUMN created_at timestamptz NOT NULL DEFAULT now();
-- У старых результатов нет собственной даты: ближайшая известная дата не переносит всю историю в текущий месяц.
UPDATE test_runs tr SET created_at = COALESCE(s.finished_at, s.started_at, s.created_at)
FROM submissions s WHERE s.id = tr.submission_id;
CREATE INDEX test_runs_created_idx ON test_runs (created_at, submission_id);
-- Резерв учитывается до появления test_runs: очередь не позволяет потратить остаток несколько раз.
ALTER TABLE submissions ADD COLUMN test_run_budget integer NOT NULL DEFAULT 0 CHECK (test_run_budget >= 0);

-- +goose Down
-- Обратные миграции запрещены.
