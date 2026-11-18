-- +goose Up

CREATE TABLE feedback_requests (
    id                uuid PRIMARY KEY DEFAULT uuidv7(),
    user_id           uuid        NOT NULL REFERENCES users (id),
    type              integer     NOT NULL CHECK (type BETWEEN 1 AND 3),
    language_id       text        REFERENCES languages (id),
    title             text        NOT NULL CHECK (char_length(title) BETWEEN 1 AND 200),
    text              text        NOT NULL CHECK (char_length(text) BETWEEN 1 AND 10000),
    status            integer     NOT NULL DEFAULT 1 CHECK (status BETWEEN 1 AND 4),
    admin_comment     text        NOT NULL DEFAULT '' CHECK (char_length(admin_comment) <= 5000),
    client_request_id uuid,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- Ключ привязан к автору, как у посылок: параллельные повторы не создают копий.
CREATE UNIQUE INDEX feedback_requests_ukey_user_id_client_request_id
    ON feedback_requests (user_id, client_request_id) WHERE client_request_id IS NOT NULL;

-- UUIDv7 задаёт порядок keyset-пагинации; индексы покрывают каждый набор фильтров.
CREATE INDEX feedback_requests_idx_user_id_id ON feedback_requests (user_id, id DESC);
CREATE INDEX feedback_requests_idx_status_id ON feedback_requests (status, id DESC);
CREATE INDEX feedback_requests_idx_type_id ON feedback_requests (type, id DESC);
CREATE INDEX feedback_requests_idx_status_type_id ON feedback_requests (status, type, id DESC);
CREATE INDEX feedback_requests_idx_user_id_status_id ON feedback_requests (user_id, status, id DESC);
CREATE INDEX feedback_requests_idx_user_id_type_id ON feedback_requests (user_id, type, id DESC);
CREATE INDEX feedback_requests_idx_user_id_status_type_id ON feedback_requests (user_id, status, type, id DESC);

-- +goose Down
-- Обратные миграции запрещены.
