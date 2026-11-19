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
internal/          auth (Keycloak JWT), server, user, problem, submission, feedback, billing,
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

## ИИ-генерация тестов (HPRPR-10)

`TestGenerationService` доступен роли `author` для своего задания и `admin` для любого.
Новые генерации требуют активную подписку **Pro**, в том числе для администратора.
`plans.daily_test_generation_limit` задаёт дневную квоту: free=0, pro=10. Сутки — UTC,
очередь и неудачные генерации тоже расходуют квоту. `ResourceExhausted` содержит
`google.rpc.RetryInfo` до следующих суток. Повтор `request_id` квоту не расходует.

- `POST /v1/problems/{problem_id}/test-generations` — `request_id` (обязательный UUID),
  `language_id`, `source` эталона автора (UTF-8 ≤64 КиБ), `test_count` (1–20), `wishes`
  (необязательно, до 4000 символов). Язык должен быть активен и разрешён в задании.
- `GET /v1/problems/{problem_id}/test-generations/{id}` — `generation.status`
  (`QUEUED`, `RUNNING`, `DONE`, `FAILED`), `tests`, безопасная общая `error`.
  У кандидата есть `id`, `input` и `expected` (bytes, base64 в REST), `explanation`,
  `rejection_reason` и `is_accepted`. Отброшенные кандидаты не содержат вход/выход.
- `POST /v1/problems/{problem_id}/test-generations/{id}:accept` — `test_ids`, непустой
  список до 20 UUID пригодных кандидатов. Создаёт обычные готовые **закрытые** тесты
  с одним баллом и `is_generated=true`; они участвуют в следующих посылках.
  Повторное принятие безопасно; можно принять другую часть той же генерации.
  Если принятый тест впоследствии удалён обычным API, повтор accept его не воссоздаёт.

Пример тела запуска:

```json
{
  "request_id": "019a9b25-b420-7000-8000-000000000010",
  "language_id": "python3.14",
  "source": "a, b = map(int, input().split())\nprint(a + b)",
  "test_count": 5,
  "wishes": "Нулевые и отрицательные значения, один большой вход"
}
```

Отдельный пул в `cmd/worker` использует таблицу `test_generations` как очередь
`SKIP LOCKED`: аренда 60s, heartbeat 20s, новый токен при перехвате, максимум три
попытки после падений процесса. SIGTERM возвращает аренду без расхода попытки.
Пул использует `WORKER_CONCURRENCY` и `WORKER_POLL_INTERVAL`, независимо от пула
посылок: ожидание LLM не занимает слоты judge. Попытка ограничена 10 минутами.
Обычная ошибка LLM/песочницы завершает генерацию с `FAILED`; повторы HTTP 429/5xx
выполняет сам LLM-клиент в пределах общего deadline. Состояние и черновики
фиксируются атомарно с проверкой токена аренды.

В очереди хранится снимок условия, исходника и команд языка. Внешнему провайдеру
передаются только условие и эталон **автора**, без данных пользователя. Недоверенные
условие/исходник/пожелания сериализованы как данные в отдельном user-сообщении;
системный промпт запрещает следовать инструкциям в этих данных. Это уменьшает риск
инъекции; ответы всё равно считаются недоверенными. Исходник удаляется из снимка
после завершения. Ни промпт, ни ответ, ни ключ не записываются в логи.

Модель предлагает конкретные небольшие входы и граничные случаи либо Python-скрипт
для одного большого входа. Генератор запускается отдельно от эталона, без сети:
3s, 128 МиБ, 16 процессов, ≤64 КиБ исходник, ≤1 МиБ вывод. Пустой/пробельный или
слишком большой вход отбрасывается. Эталон компилируется с лимитами judge (30s,
1 ГиБ); для каждого входа применяются лимиты задания и общий лимит вывода 1 МиБ.
Падение/TL/ML/превышение вывода отбрасывает кандидат с причиной. Модель не определяет
ожидаемый ответ: это stdout эталона. Семантическую корректность эталона подтверждает
автор, просматривая черновики. Условие не содержит универсального формального
описания входа, поэтому проверяются размер/непустота и исполнение эталона.

До accept данные находятся только в `generated_tests`, после — файлы в S3
`problems/{problem_id}/tests/{draft_id}/{input,expected}` и строки `test_cases`.
Приём сначала проверяет весь выбор и загружает файлы по стабильным ключам, затем
одной транзакцией добавляет тесты; сетевых вызовов в транзакции нет. При сбое S3
тесты не включаются. Повтор после частичной загрузки/потери ответа использует те же
ключи. Прерванная загрузка может оставить объекты до повторного accept или удаления
задания (его обычная очистка префикса удаляет их).
Черновики хранят до удаления задания; TTL и автоочистка не реализованы.
Суммарный ответ ограничен 20 кандидатами по 1 МиБ вход/выход; gateway допускает
ответы gRPC до 48 МиБ. Внешним gRPC-клиентам для больших черновиков тоже нужно
увеличить receive limit.

Общий клиент [internal/llm](internal/llm/client.go) переиспользуется для подсказок
и подбора: `Chat(ctx, []Message, Options)` возвращает проверенный JSON-объект,
`Embed(ctx, []string, Options)` — векторы в порядке входов. `Options.JSONSchema`
выбирает `json_schema`, иначе используется `json_object`. `Options.ZDR` явно
передаёт `provider.zdr`, в том числе для embeddings; для кода студентов вызывающий
обязан выбрать `true`. Клиент не ослабляет этот параметр при повторе.
Форматы сверены с официальной документацией OpenRouter:
[structured outputs](https://openrouter.ai/docs/guides/features/structured-outputs),
[ZDR](https://openrouter.ai/docs/guides/features/zdr),
[Retry-After](https://openrouter.ai/docs/api_reference/errors-and-debugging).
Повторы 429/5xx используют экспоненциальную задержку и учитывают `Retry-After`
(секунды или HTTP-дата), а также JSON `retry_after` (секунды). Ответы/ошибки
провайдера не включаются в сообщения ошибок приложения. Пустой, усечённый или
некорректный JSON — ошибка. Конфигурация — `LLM_*` в `configs/.env.example`.
Без ключа только создание генераций отвечает `Unavailable`; чтение и принятие
сохранённых черновиков остаются доступны. Бесплатные модели предназначены для
разработки; production-модель и требования к приватности выбирает оператор.

Проверка (отдельный PostgreSQL на 55453 и тестовый SeaweedFS на 58333):

```sh
DATABASE_URL=postgres://autocheck:autocheck@localhost:55453/autocheck?sslmode=disable \
S3_TEST_ENDPOINT=localhost:58333 go test -race -count=1 ./...
```

`TestDockerGatewayPipeline` проходит через настоящий REST→gRPC/JWT, очередь,
PostgreSQL, Docker и SeaweedFS; только LLM и издатель JWKS локальные. Проверяются
Python/C++, генератор, запрет сети, отбрасывание падающего эталона и повтор accept.
Без `S3_TEST_ENDPOINT` только этот тест пропускается; остальные DB/Docker-тесты
требуют окружение проекта. `TestOpenRouterLive` делает один запрос без повторов
при `OPENROUTER_API_KEY` или `LLM_API_KEY`, иначе пропускается. Не запускайте его
многократно: бесплатная квота мала.
