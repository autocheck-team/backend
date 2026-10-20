-- +goose Up

-- Файлы тестов загружаются клиентом напрямую в S3 по presigned-ссылкам, поэтому
-- тест сначала создаётся «пустым», а после загрузки подтверждается: бэкенд проверяет,
-- что оба объекта есть, и запоминает размеры. В проверке участвуют только is_ready тесты.
ALTER TABLE test_cases
    ADD COLUMN is_ready      boolean     NOT NULL DEFAULT false,
    ADD COLUMN input_size    bigint      NOT NULL DEFAULT 0 CHECK (input_size >= 0),
    ADD COLUMN expected_size bigint      NOT NULL DEFAULT 0 CHECK (expected_size >= 0),
    ADD COLUMN created_at    timestamptz NOT NULL DEFAULT now();

CREATE INDEX test_cases_ready_idx ON test_cases (problem_id) WHERE is_ready;

-- +goose Down

DROP INDEX test_cases_ready_idx;
ALTER TABLE test_cases
    DROP COLUMN is_ready,
    DROP COLUMN input_size,
    DROP COLUMN expected_size,
    DROP COLUMN created_at;
