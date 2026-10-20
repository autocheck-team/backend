-- +goose Up

-- Ключ идемпотентности отправки: повтор запроса (например, после обрыва сети)
-- с тем же client_request_id возвращает уже созданную посылку, а не создаёт вторую.
ALTER TABLE submissions ADD COLUMN client_request_id uuid;
CREATE UNIQUE INDEX submissions_client_request_key
    ON submissions (user_id, client_request_id) WHERE client_request_id IS NOT NULL;

-- +goose Down

DROP INDEX submissions_client_request_key;
ALTER TABLE submissions DROP COLUMN client_request_id;
