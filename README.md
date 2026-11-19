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
internal/          auth (Keycloak JWT), server, user, problem, submission, feedback, billing, material,
                   storage (S3), sandbox (Docker), config, db (+ migrations)
```

## API и авторизация

Сервисы описаны в proto, REST получается из аннотаций `google.api.http` через grpc-gateway.
Шлюз ходит в gRPC-сервер по сети, поэтому REST и gRPC проходят одни и те же интерсепторы:
авторизация, валидация (protovalidate), перевод внутренних ошибок в `Internal`.

Каждый запрос, кроме health-проверок, списка тарифов и вебхука ЮKassa, требует access-токен Keycloak в заголовке
`Authorization: Bearer <token>`. Бэкенд проверяет подпись по JWKS, `iss` и `aud = autocheck-api`,
а роли берёт из `realm_access.roles` (`student`, `author`, `admin`). При первом запросе
пользователь появляется в таблице `users`.

После изменения proto: `make proto-gen` (нужен [buf](https://buf.build/docs/installation)),
сгенерированный код коммитится вместе с proto. CI проверяет, что он актуален.

### Запросы пользователей (HPRPR-4)

`FeedbackService` принимает предложения об обновлении компилятора/языка
(`FEEDBACK_REQUEST_TYPE_COMPILER_UPDATE`), новой библиотеке (`FEEDBACK_REQUEST_TYPE_NEW_LIBRARY`)
и прочих изменениях (`FEEDBACK_REQUEST_TYPE_OTHER`).

- `POST /v1/feedback-requests` — создание любым вошедшим пользователем: `type`, `title`
  (1–200 символов), `text` (1–10000 символов), необязательный `language_id` из `languages`
  (включая выключенные языки). Заголовок и текст не могут состоять только из пробелов.
  Необязательный UUID `client_request_id` работает как у посылок: повтор от того же
  пользователя возвращает первоначальный результат, даже если содержимое изменилось.
- `GET /v1/feedback-requests` — свои запросы; для `admin` — все. Фильтры `status` и `type`
  принимают enum, `UNSPECIFIED` означает отсутствие фильтра. `page_size` по умолчанию 20,
  максимум 100; `page_token` берётся из `next_page_token`. Порядок — новые первыми;
  при продолжении списка следует сохранять фильтры.
- `GET /v1/feedback-requests/{id}` — свой запрос или любой для `admin`.
  Чужой запрос возвращает 404, в том числе для роли `author`.
- `PATCH /v1/feedback-requests/{id}` — только `admin`: обязательный `status` и
  `admin_comment` до 5000 символов. Комментарий заменяется целиком, пустое значение
  очищает его. Статусы: `NEW → ACCEPTED | REJECTED`, `ACCEPTED → COMPLETED | REJECTED`.
  Повтор текущего статуса разрешён для исправления комментария; конечные статусы
  не переоткрываются. Запрещённый переход — 400 (`FailedPrecondition`), конфликт
  параллельных изменений — 409 (`Aborted`), запрос можно повторить после чтения.

Сервер выставляет автора, начальный статус `NEW`, `create_time` и `update_time`.
Каждая операция БД ограничена `FEEDBACK_QUERY_TIMEOUT` (по умолчанию `5s`, значение
должно быть положительным; пример — `configs/.env.example`). Области feedback не
нужны S3 и воркер. Интеграционные тесты репозитория и REST-шлюза запускаются при
заданном `DATABASE_URL`; JWT подписывает локальный тестовый издатель с JWKS.

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

Рабочее пространство ограничено 256 МиБ и 16384 inode (tmpfs volume с `nosuid,nodev`),
один файл — 64 МиБ, `/tmp` — 16 МиБ. Лимит stdout и stderr общий: 16 МиБ;
превышение даёт RE. Docker не сохраняет дополнительную копию вывода в своих логах.
Отдельный служебный контейнер удерживает tmpfs между компиляцией и тестами и удаляется
вместе с workspace. Его лимиты — 16 МиБ и 4 процесса; файлы tmpfs также расходуют RAM хоста.
Песочница использует встроенный профиль seccomp на основе Moby с запретом ptrace и
`process_vm_*`; `/proc/1/environ` маскируется. Обновление профиля требует повторного
прогона поддерживаемых языков и проверок безопасности.

`make test-sandbox-security` проверяет атаки на настоящем Docker через тот же Runner,
что использует воркер. Цель требует доступный Engine; отдельные Go-тесты, как и другие
Docker-тесты проекта, пропускаются без Docker. БД, S3 и Keycloak для этой матрицы не нужны.
Сценарии, наблюдения и границы проверки: [безопасность песочницы](docs/sandbox-security.md).

## Статус посылки без поллинга

`GET /v1/submissions/{id}:watch` (`WatchSubmission`, server streaming) сразу отдаёт
текущее состояние, затем изменения и закрывает поток после `done` или `failed`.
Права совпадают с `GetSubmission`: владелец посылки, автор задания, администратор.
Воркер публикует `pg_notify('submission_events', id)` в транзакции результата.
Каждый API держит одно отдельное LISTEN-соединение, переподключается при обрыве
и перечитывает БД по сигналу. Это поток актуальных снимков, а не журнал всех переходов:
несколько быстрых изменений могут объединиться в последнее состояние.

Максимальная длительность подписки — 10 минут, затем приходит `DeadlineExceeded`
и фронт может подключиться заново. Внутренняя сверка с БД каждые 30 секунд
страхует пропущенный сигнал, в том числе при обрыве соединения. Неизменившиеся
снимки не отправляются. JSON-heartbeat не нужен: gateway сразу flush-ит сообщения,
HTTP-сервер не ограничивает время записи потока. У внешнего reverse proxy нужно
отключить буферизацию и установить idle timeout больше 10 минут.

Gateway возвращает newline-delimited JSON: `{"result":{"submission":...}}`.
Ошибка после начала потока приходит отдельной строкой с `error` (HTTP-код уже не меняется).
Для заголовка Authorization используется `fetch`:

```js
async function watchSubmission(id, token, onSubmission, signal) {
  const response = await fetch(`/v1/submissions/${encodeURIComponent(id)}:watch`, {
    headers: { Authorization: `Bearer ${token}` },
    signal, // AbortController отменяет подписку при уходе со страницы.
  });
  if (!response.ok) throw new Error(`HTTP ${response.status}`);
  const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
  let pending = "";
  function consume(line) {
    if (!line.trim()) return;
    const message = JSON.parse(line);
    if (message.error) throw new Error(message.error.message);
    onSubmission(message.result.submission);
  }
  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      pending += value;
      let end;
      while ((end = pending.indexOf("\n")) !== -1) {
        consume(pending.slice(0, end));
        pending = pending.slice(end + 1);
      }
    }
    consume(pending);
  } finally {
    await reader.cancel();
    reader.releaseLock();
  }
}
```

При сетевой ошибке или `DeadlineExceeded` фронт переподключается с задержкой:
первый снимок восстановит состояние. После финального статуса подписку повторять
не нужно. Просроченный access-токен перед новым подключением обновляется.

## Отчёт по посылке

`GET /v1/submissions/{id}/report` (`GetSubmissionReport`) возвращает JSON с `languageId`,
`status`, `verdict`, `score`, `compileLog` (только CE) и `testRuns` по ordinal.
Права совпадают с получением посылки. До конца проверки отчёт может быть неполным.
У каждого результата есть вердикт, wall/CPU-время в миллисекундах, память в КиБ,
`isTimeLimitExceeded` (TL) и `isMemoryLimitExceeded` (ML).

Только у открытого теста (`isSample`) с WA появляются `input`, `expectedOutput`,
`actualOutput`: каждый содержит `text` до 4096 байт UTF-8 и `isTruncated`.
Невалидный UTF-8 заменяется на U+FFFD. `diff` сравнивает показанные префиксы построчно:
пробел — совпадение, `-` — строка эталона, `+` — строка решения; отсутствие последнего
LF отмечается отдельно, CRLF сохраняется. При обрезке это diff префиксов, а не полных файлов.
Для старого прогона без `stdout_key` фактический вывод и diff отсутствуют.
Закрытые тесты не раскрывают вход, эталон, stdout или diff даже автору/администратору.
Отчёт собирается из БД и S3 по запросу; `report_key` для него не используется.

## Тарифы, подписки и оплата

Каталог хранится в `plans`: начальные тарифы `free` (0 ₽, 1 компиляция за 60 секунд,
100 тестовых прогонов за календарный месяц UTC) и `pro` (299 ₽ за 30 дней, без лимитов).
При отсутствии активной подписки или после её окончания действует `free`.
Прогон — запись `test_runs`, месяц определяется по времени прогона, а не создания посылки.
Компиляция учитывается при приёме посылки с непустым `languages.compile_cmd`, включая CE.
Очередь резервирует число готовых тестов; если остатка на всё задание недостаточно,
посылка отклоняется. Завершение посылки освобождает неиспользованный резерв.
Повтор создания посылки с прежним `client_request_id` не тратит квоту.

REST API (gRPC `BillingService`):

- `GET /v1/plans` — публичный каталог цен и лимитов.
- `GET /v1/subscriptions/me` — эффективный тариф, статус, срок, использованные прогоны,
  остатки с учётом очереди и время ожидания компиляции. `-1` означает отсутствие лимита.
- `POST /v1/subscriptions:checkout` — `{ "plan_code": "pro", "return_url": "https://…",
  "request_id": "UUID" }`, ответ содержит `payment` и `confirmation_url` для редиректа.
  При сетевой ошибке повторять **тот же** `request_id` с прежними параметрами.
- `GET /v1/payments/me?page_size=20&page_token=…` — платежи текущего пользователя,
  keyset-пагинация через `next_page_token`.
- `POST /v1/billing/yookassa/webhook` — публичное уведомление ЮKassa. Статус и сумма
  из тела игнорируются: оплата проверяется через GET API ЮKassa, включая сумму,
  валюту, локальные метаданные и признак `paid`. Успех активирует/продлевает доступ
  ровно один раз; переход на `return_url` сам по себе доступа не даёт.

Превышение компиляций возвращает gRPC `ResourceExhausted` / HTTP 429 с `google.rpc.RetryInfo`.
При исчерпании прогонов возвращается 429 с предложением оформить подписку.
Проверка квот и постановка в очередь выполняются одной транзакцией под блокировкой
пользователя, поэтому лимиты работают при нескольких экземплярах API.

Настройки окружения (пример — [configs/.env.example](configs/.env.example)):

| Переменная | По умолчанию | Назначение |
| --- | --- | --- |
| `YOOKASSA_SHOP_ID` | пусто | ID тестового магазина |
| `YOOKASSA_SECRET_KEY` | пусто | Секретный ключ; не записывать в логи |
| `YOOKASSA_API_URL` | `https://api.yookassa.ru/v3` | Тестовый магазин использует этот же API |
| `YOOKASSA_TIMEOUT` | `10s` | Дедлайн каждого обращения к провайдеру |
| `BILLING_SYNC_INTERVAL` | `30s` | Период фоновой проверки pending-платежей в API |

Без полного набора ключей checkout выключен (`Unavailable` / HTTP 503),
`checkout_enabled=false`; каталог, подписка и бесплатные квоты продолжают работать.
Локально и в Codespaces входящий webhook необязателен: API сам сверяет pending-платежи,
включая потерянный ответ создания. Намерение и ключ идемпотентности сохраняются до POST.
Неопределённое создание старше 23 часов требует ручной сверки: повтор POST после
истечения 24-часового срока ключа ЮKassa мог бы создать вторую оплату.
Это разовые покупки периода, автоматического списания и сохранения карты нет.
Протокол оплаты: [документация ЮKassa](https://yookassa.ru/developers/payment-acceptance/getting-started/quick-start).

Тесты биллинга используют `httptest` и настоящий PostgreSQL через `DATABASE_URL`.
`go test -race -count=1 -run TestYookassaLive -v ./internal/billing` дополнительно создаёт
неподтверждённый платёж тестового магазина, проверяет повтор с тем же ключом и GET.
Без `YOOKASSA_SHOP_ID` / `YOOKASSA_SECRET_KEY` этот тест пропускается. Оплата тестом
не подтверждается; pending-платёж впоследствии отменяется самим провайдером.

## Учебные материалы (HPRPR-7)

Материалы платформы связаны с одним или несколькими заданиями. `MaterialService`
доступен через gRPC и REST с теми же Keycloak JWT и валидацией, что остальные API.

- `POST /v1/materials` — только admin: `material` с `title`, `description`,
  `content_markdown`, `problem_ids` (1–100 UUID), `ordinal` (от 0), `is_published`;
  обязательный UUID `request_id` обеспечивает идемпотентное создание.
- `PUT /v1/materials/{id}` — только admin: полная замена `material`, включая
  публикацию и связи с заданиями. Поле `id` берётся из пути. Черновик и его описание
  видит только admin, в том числе при наличии Pro у обычного пользователя.
- `DELETE /v1/materials/{id}` — только admin: удаление вместе с вложениями.
  Повтор безопасен и позволяет завершить очистку S3 после временного сбоя.
- `GET /v1/materials` — описания для любого вошедшего; необязательный `problem_id`.
  `GET /v1/problems/{problem_id}/materials` — тот же список с фильтром задания.
  `page_size` по умолчанию 20, максимум 100; `next_page_token` передаётся как
  `page_token`. Порядок: `ordinal ASC, id ASC`, keyset без OFFSET. При продолжении
  сохраняйте фильтр; изменение порядка/связей администратором может переместить
  запись относительно курсора. Списки никогда не возвращают Markdown и вложения.
- `GET /v1/materials/{id}` — описание и `is_locked` для любого вошедшего.
  При активной подписке с `plan.has_material_access=true` (Pro) или роли admin
  возвращаются также `content_markdown` и подтверждённые вложения с GET-ссылками.
  Для free, отменённой, будущей или истёкшей подписки `is_locked=true`, Markdown
  пустой, вложений нет. Выбран единый ответ с `is_locked`, чтобы клиент мог показать
  описание и предложение «доступно по подписке Pro» без повторного запроса.
  Полномочия автора задания сами по себе платного доступа не дают.

Вложения доступны только admin для изменения:

1. `POST /v1/materials/{material_id}/attachments` с `filename` и UUID `request_id`
   возвращает `attachment.id`, `upload_url`, `upload_expire_time`, `max_size_bytes`.
   Повтор операции возвращает то же вложение; после confirm PUT-ссылка больше
   не выдаётся. Имя файла не должно содержать путь или управляющие символы.
2. Загрузите файл HTTP PUT по `upload_url` (срок 15 минут).
3. `POST /v1/materials/{material_id}/attachments/{id}:confirm` с пустым JSON `{}`
   проверяет непустой файл, размер и сигнатуру формата: PDF, PNG, JPEG, GIF, WebP.
   SVG/HTML не принимаются. Подтверждение читает не более лимита + 1 байт и сохраняет
   закрытую копию; последующий PUT по старой ссылке не меняет выдаваемый файл.
   Повтор confirm сохраняет первую подтверждённую копию.
4. `GET /v1/materials/{id}` выдаёт ссылки на подтверждённые вложения при доступе.
   Ссылки действуют 5 минут, `download_expire_time` показывает срок. Уже выданная
   ссылка остаётся действующей до истечения срока даже после завершения подписки.
   Admin дополнительно видит метаданные ожидающих confirm вложений без GET-ссылок.
5. `DELETE /v1/materials/{material_id}/attachments/{id}` удаляет вложение и объекты;
   повтор безопасен. Бакет должен оставаться закрытым от анонимного чтения.

Лимит применяется при confirm, а не при загрузке PUT в S3: неподтверждённые файлы
не выдаются пользователям. Для удаления заброшенных/повторно загруженных staging-файлов
можно настроить lifecycle хранилища на ключи `materials/*/attachments/*/upload`;
подтверждённые копии в `confirmed/` под это правило попадать не должны.

Настройки в `configs/.env.example`: `MATERIAL_QUERY_TIMEOUT=5s`,
`MATERIAL_OBJECT_TIMEOUT=30s`, `MATERIAL_MAX_ATTACHMENT_BYTES=10485760` (10 МиБ;
допустимы значения 1–67108864). Используется существующее подключение `S3_*`.
Флаг `has_material_access` также виден в каталоге тарифов и текущей подписке.

Тесты `internal/material` используют настоящий PostgreSQL (`DATABASE_URL`), локальный
JWT/JWKS и сетевой REST→gRPC gateway. `TestGatewayMaterialS3` автоматически поднимает
SeaweedFS 4.53 через Docker на динамическом localhost-порту и удаляет свой контейнер.
Проверяются PUT/confirm/GET, неизменяемость подтверждённого файла и очистка S3.
