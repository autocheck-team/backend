# backend

[![CI](https://github.com/autocheck-team/backend/actions/workflows/ci.yml/badge.svg)](https://github.com/autocheck-team/backend/actions/workflows/ci.yml)

Бэкенд системы автоматической проверки решений: HTTP API, воркер проверки посылок и миграции БД.

- **Стек:** Go 1.27, PostgreSQL 18 (pgx, goose), SeaweedFS (S3)
- **Архитектура системы:** [infra/docs/architecture.md](https://github.com/autocheck-team/infra/blob/main/docs/architecture.md)
- **Схема БД:** [docs/database.md](docs/database.md)
- **Задачи:** [GitHub Project](https://github.com/orgs/autocheck-team/projects)

## Структура

```
cmd/api        HTTP API
cmd/worker     воркер проверки посылок
cmd/migrate    миграции БД (up / down)
internal/      config, db (+ migrations), httpapi
```

## Разработка

Нужны Go 1.27 и Docker. Postgres и SeaweedFS поднимаются из репозитория
[infra](https://github.com/autocheck-team/infra):

```sh
# в infra
docker compose up -d postgres seaweedfs

# здесь
make migrate
make run-api       # http://localhost:8080/readyz
make test          # юнит-тесты + интеграционные тесты схемы на локальном Postgres
make lint
```

Образы `api`, `worker` и `migrate` собираются из одного `Dockerfile` (`--build-arg CMD=...`)
и публикуются в GHCR при каждом мёрже в `main`: `ghcr.io/autocheck-team/backend-<cmd>`.

## Как вносить изменения

Ветка от `main` → PR → зелёный CI → squash merge. Прямые пуши в `main` запрещены.
Подробнее: [CONTRIBUTING](https://github.com/autocheck-team/.github/blob/main/CONTRIBUTING.md).
