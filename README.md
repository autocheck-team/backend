# backend

[![CI](https://github.com/autocheck-team/backend/actions/workflows/ci.yml/badge.svg)](https://github.com/autocheck-team/backend/actions/workflows/ci.yml)

Бэкенд системы автоматической проверки решений: gRPC API с REST-шлюзом, воркер проверки посылок и миграции БД.

- **Стек:** Go 1.27, gRPC + grpc-gateway, buf, protovalidate, PostgreSQL 18 (pgx, goose), SeaweedFS (S3), Keycloak (OIDC)
- **Контракт API:** [api/autocheck/v1](api/autocheck/v1) (proto), [api/openapi/autocheck.swagger.yaml](api/openapi/autocheck.swagger.yaml) (REST), он же отдаётся на `GET /openapi.yaml`
- **Архитектура системы:** [infra/docs/architecture.md](https://github.com/autocheck-team/infra/blob/main/docs/architecture.md)
- **Схема БД:** [docs/database.md](docs/database.md)
- **Задачи:** [GitHub Project](https://github.com/orgs/autocheck-team/projects)

## Структура

```
api/autocheck/v1   proto-контракты (источник правды для API)
api/openapi        сгенерированная OpenAPI-спецификация REST
pkg/api            сгенерированный Go-код (protobuf, gRPC, gateway); руками не править
cmd/api            gRPC :9090 + REST-шлюз :8080
cmd/worker         воркер проверки посылок
cmd/migrate        миграции БД (up / down)
internal/          auth (Keycloak JWT), server, user, problem, submission,
                   storage (S3), sandbox (Docker), config, db (+ migrations)
```

## API и авторизация

Сервисы описаны в proto, REST получается из аннотаций `google.api.http` через grpc-gateway.
Шлюз ходит в gRPC-сервер по сети, поэтому REST и gRPC проходят одни и те же интерсепторы:
авторизация, валидация (protovalidate), перевод внутренних ошибок в `Internal`.

Каждый запрос, кроме health-проверок, требует access-токен Keycloak в заголовке
`Authorization: Bearer <token>`. Бэкенд проверяет подпись по JWKS, `iss` и `aud = autocheck-api`,
а роли берёт из `realm_access.roles` (`student`, `author`, `admin`). При первом запросе
пользователь появляется в таблице `users`.

После изменения proto: `make proto-gen` (нужен [buf](https://buf.build/docs/installation)),
сгенерированный код коммитится вместе с proto. CI проверяет, что он актуален.

## Разработка

Нужны Go 1.27 и Docker. Postgres и SeaweedFS поднимаются из репозитория
[infra](https://github.com/autocheck-team/infra):

```sh
# в infra
docker compose up -d postgres seaweedfs keycloak

# здесь
make migrate
OIDC_ISSUER=http://localhost:8180/realms/autocheck make run-api

# токен тестового пользователя (student|author|admin, пароль совпадает с ролью)
TOKEN=$(curl -s -d grant_type=password -d client_id=autocheck-cli \
  -d username=author@autocheck.local -d password=author \
  http://localhost:8180/realms/autocheck/protocol/openid-connect/token | jq -r .access_token)
curl -H "Authorization: Bearer $TOKEN" localhost:8080/v1/me
make test          # юнит-тесты + интеграционные тесты схемы на локальном Postgres
make lint
```

Образы `api`, `worker` и `migrate` собираются из одного `Dockerfile` (`--build-arg CMD=...`)
и публикуются в GHCR при каждом мёрже в `main`: `ghcr.io/autocheck-team/backend-<cmd>`.

## Как вносить изменения

Ветка от `main` → PR → зелёный CI → squash merge. Прямые пуши в `main` запрещены.
Подробнее: [CONTRIBUTING](https://github.com/autocheck-team/.github/blob/main/CONTRIBUTING.md).

## Проверка посылок

`make run-worker` запускает пул проверок. Воркеру нужны `DATABASE_URL`, доступ к S3
(те же `S3_*`, что у API) и Docker Engine через сокет либо `DOCKER_HOST`.
`WORKER_CONCURRENCY` задаёт размер пула (по умолчанию 2), `WORKER_POLL_INTERVAL` —
интервал опроса пустой очереди (1s), `WORKER_ID` — имя воркера (hostname),
`SANDBOX_RUNTIME` — необязательный OCI runtime, например `runsc`.
Воркер сам скачивает образы из `languages`; запуск образов требует доступа Docker
к registry. Скачивание ограничено отдельным таймаутом 3 минуты.

Очередь `judge_jobs` работает через PostgreSQL `FOR UPDATE SKIP LOCKED`: аренда 60s,
heartbeat 20s. Просроченную аренду забирает другой воркер. Этапы посылки:
`queued → compiling → running → done` (CE завершает проверку после компиляции).
Системные ошибки повторяются с задержками 2s и 4s, максимум 3 попытки; затем
`failed / SE`. Если готовых тестов нет, SE выставляется сразу. SIGTERM отменяет
проверки и возвращает задания без расхода попытки. Результат и удаление задания
из очереди записываются одной транзакцией.

Проверяются все готовые тесты по ordinal; `stop_on_first_failure` позволяет
остановиться на первой ошибке. Эталон никогда не монтируется решению, вход идёт
через stdin. Custom-чекер — Python-скрипт: автор получает PUT-ссылку через
`POST /v1/problems/{problem_id}/checker:upload`, загружает файл и вызывает
`POST /v1/problems/{problem_id}/checker:confirm`. Без подтверждённого чекера
custom-задание не публикуется. Скрипт запускается в отдельном workspace с аргументами
`input expected output`: exit 0 = OK, 1 = WA, остальные исходы = системная ошибка.

CPU-время и пик памяти приблизительны: Docker stats опрашивается каждые 100ms,
короткие всплески могут быть пропущены. Память считается без inactive_file.
У решения есть wall timeout с запасом 300ms на старт и страховочный RLIMIT_CPU.
Закрытые тесты не раскрывают stderr; stdout сохраняется только у sample-тестов.

Для ограничения памяти самого воркера он читает файлы тестов размером до 64 МБ
каждый и скрипты чекеров до 1 МБ. Превышение считается ошибкой данных задания.
