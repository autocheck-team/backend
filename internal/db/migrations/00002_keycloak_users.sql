-- +goose Up

-- Аутентификацией занимается Keycloak: пароли и refresh-токены у него.
-- users остаётся локальной копией профиля, чтобы задания и посылки ссылались
-- на пользователя. id совпадает с claim sub из токена Keycloak.
DROP TABLE refresh_tokens;
ALTER TABLE users
    DROP COLUMN password_hash,
    ALTER COLUMN id DROP DEFAULT;

COMMENT ON TABLE users IS 'Профиль пользователя из Keycloak, обновляется при каждом входе';
COMMENT ON COLUMN users.id IS 'sub из токена Keycloak';
COMMENT ON COLUMN users.role IS 'Старшая роль из realm_access.roles: admin > author > student';

-- +goose Down

ALTER TABLE users
    ADD COLUMN password_hash text NOT NULL DEFAULT '',
    ALTER COLUMN id SET DEFAULT uuidv7();
ALTER TABLE users ALTER COLUMN password_hash DROP DEFAULT;

CREATE TABLE refresh_tokens (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash bytea       NOT NULL UNIQUE,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX refresh_tokens_user_idx ON refresh_tokens (user_id);
