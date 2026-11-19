-- +goose Up

-- UUIDv7 повторяет идентификаторы пользователей и посылок существующего API.
CREATE TABLE playground_projects (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    user_id uuid NOT NULL REFERENCES users (id),
    title text NOT NULL CHECK (char_length(title) BETWEEN 1 AND 200),
    language_id text NOT NULL REFERENCES languages (id),
    files jsonb NOT NULL CHECK (jsonb_typeof(files) = 'array' AND jsonb_array_length(files) BETWEEN 1 AND 20),
    client_request_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz,
    CONSTRAINT playground_projects_ukey_user_id_client_request_id UNIQUE (user_id, client_request_id)
);
CREATE INDEX playground_projects_idx_user_id_id ON playground_projects (user_id, id DESC) WHERE deleted_at IS NULL;

-- Снимок не меняется при редактировании или удалении проекта; расход квоты тоже сохраняется.
CREATE TABLE playground_runs (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    project_id uuid NOT NULL REFERENCES playground_projects (id),
    user_id uuid NOT NULL REFERENCES users (id),
    language_id text NOT NULL REFERENCES languages (id),
    language jsonb NOT NULL,
    is_compiled boolean NOT NULL,
    files jsonb NOT NULL,
    stdin text NOT NULL CHECK (octet_length(stdin) <= 65536),
    client_request_id uuid NOT NULL,
    status integer NOT NULL DEFAULT 1 CHECK (status BETWEEN 1 AND 5),
    verdict text NOT NULL DEFAULT '' CHECK (verdict IN ('', 'OK', 'CE', 'TL', 'ML', 'RE', 'SE')),
    exit_code integer,
    stdout text NOT NULL DEFAULT '' CHECK (octet_length(stdout) <= 65536),
    stderr text NOT NULL DEFAULT '' CHECK (octet_length(stderr) <= 65536),
    compile_log text NOT NULL DEFAULT '' CHECK (octet_length(compile_log) <= 65536),
    is_output_truncated boolean NOT NULL DEFAULT false,
    time_ms bigint NOT NULL DEFAULT 0 CHECK (time_ms >= 0),
    memory_kb bigint NOT NULL DEFAULT 0 CHECK (memory_kb >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT playground_runs_ukey_user_id_client_request_id UNIQUE (user_id, client_request_id),
    CONSTRAINT playground_runs_check_result CHECK ((status IN (4, 5)) = (verdict <> ''))
);
CREATE INDEX playground_runs_idx_user_id_created_at ON playground_runs (user_id, created_at) WHERE is_compiled;
CREATE TABLE playground_jobs (
    id bigint PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
    run_id uuid NOT NULL UNIQUE REFERENCES playground_runs (id),
    attempts integer NOT NULL DEFAULT 0,
    locked_by text,
    locked_until timestamptz,
    run_after timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX playground_jobs_idx_run_after_id ON playground_jobs (run_after, id);

-- +goose Down
-- Обратные миграции запрещены.
