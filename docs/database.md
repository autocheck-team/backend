# База данных и жизненный цикл посылки

Общая архитектура системы: [infra/docs/architecture.md](https://github.com/autocheck-team/infra/blob/main/docs/architecture.md).

## Жизненный цикл посылки

```mermaid
stateDiagram-v2
    [*] --> queued: POST /submissions
    queued --> compiling: воркер забрал задание
    compiling --> done: CE
    compiling --> running: скомпилировано
    running --> done: все тесты прогнаны
    compiling --> failed: SE
    running --> failed: SE
```

`failed` означает сбой самой системы (вердикт `SE`), а не ошибку студента. Такую посылку
можно перепроверить. Ограничение в БД гарантирует, что у посылок в статусах `done` и
`failed` вердикт всегда заполнен.

Вердикты: `OK`, `CE` — ошибка компиляции, `WA` — неверный ответ, `TL` — превышено время,
`ML` — превышена память, `RE` — ошибка выполнения, `SE` — сбой системы.

## Схема БД

Миграции: [`internal/db/migrations`](../internal/db/migrations).

```mermaid
erDiagram
    users ||--o{ payments : "оплаты"
    users ||--o| subscriptions : "подписка"
    plans ||--o{ payments : "снимок условий"
    plans ||--o{ subscriptions : "доступ"
    users ||--o{ refresh_tokens : ""
    users ||--o{ problems : "автор"
    users ||--o{ submissions : ""
    users ||--o{ feedback_requests : "автор запроса"
    users ||--o{ playground_projects : "владелец"
    languages ||--o{ playground_projects : "язык"
    playground_projects ||--o{ playground_runs : "неизменяемый снимок"
    users ||--o{ playground_runs : "общая квота компиляций"
    playground_runs ||--o| playground_jobs : "в очереди"
    languages |o--o{ feedback_requests : "необязательная привязка"
    problems ||--o{ test_cases : ""
    problems ||--o{ problem_languages : ""
    languages ||--o{ problem_languages : ""
    problems ||--o{ submissions : ""
    languages ||--o{ submissions : ""
    submissions ||--o{ test_runs : ""
    test_cases ||--o{ test_runs : ""
    submissions ||--o| judge_jobs : "в очереди"

    plans {
        text code PK
        bigint amount_kopecks
        int period_days
        int compilation_limit
        int compilation_window_seconds
        int monthly_test_run_limit
    }
    payments {
        uuid id PK
        uuid user_id FK
        text plan_code FK
        uuid request_id
        text provider_id UK
        uuid idempotence_key UK
        bigint amount_kopecks
        text status "pending | succeeded | canceled"
        timestamptz next_sync_at
    }
    subscriptions {
        uuid user_id PK,FK
        text plan_code FK
        text status "active | canceled"
        timestamptz starts_at
        timestamptz ends_at
    }
    users {
        uuid id PK
        text email "уникален без учёта регистра"
        text password_hash
        user_role role "student | author | admin"
    }
    problems {
        uuid id PK
        uuid author_id FK
        text slug UK
        int time_limit_ms
        int memory_limit_kb
        checker_kind checker "exact | tokens | float | custom"
        bool allow_ai_hints
        bool is_published
    }
    languages {
        text id PK "go1.27, cpp20-gcc15, ..."
        text image
        text_arr compile_cmd
        text_arr run_cmd
    }
    test_cases {
        uuid id PK
        uuid problem_id FK
        int ordinal
        text input_key
        text expected_key
        bool is_sample
        int points
    }
    submissions {
        uuid id PK
        submission_status status
        verdict verdict
        int score
        int max_time_ms
        int max_memory_kb
        text compile_log
        text report_key
    }
    test_runs {
        uuid submission_id PK
        uuid test_case_id PK
        verdict verdict
        int time_ms
        int memory_kb
        text stdout_key "только для открытых тестов"
    }
    judge_jobs {
        bigint id PK
        uuid submission_id UK
        timestamptz run_after
        int attempts
        text locked_by
    }
    feedback_requests {
        uuid id PK
        uuid user_id FK
        integer type "1 compiler_update | 2 new_library | 3 other"
        text language_id FK "необязательно"
        text title
        text text
        integer status "1 new | 2 accepted | 3 rejected | 4 completed"
        text admin_comment
        uuid client_request_id "необязательно"
        timestamptz created_at
        timestamptz updated_at
    }
```

Тарифы и подписки добавлены миграцией `00009_billing.sql`; таблицы ИИ-функций
добавляются отдельно.

## Запросы пользователей

Миграция `00008_feedback_requests.sql` добавляет `feedback_requests`. Тип и статус
хранятся как `integer` с CHECK-ограничениями; `UNSPECIFIED = 0` не сохраняется.
Автор ссылается на `users`, необязательный язык — на `languages`, включая неактивные.
БД ограничивает длины заголовка (1–200), текста (1–10000) и комментария (0–5000)
в символах. Сервис дополнительно отвергает заголовок и текст только из пробелов.

Уникальный частичный индекс `(user_id, client_request_id)` предотвращает дубликаты
даже при параллельной отправке; NULL позволяет отправлять запросы без ключа.
Индексы по `(user_id, id DESC)`, `(status, id DESC)`, `(type, id DESC)`,
`(status, type, id DESC)` и сочетаниям пользователя со статусом и/или типом
поддерживают все варианты списков. Общий список использует первичный ключ.
Keyset-пагинация сравнивает UUIDv7 (`id < cursor`); `updated_at` не влияет на порядок.

Начальный статус — `new`. Сервис разрешает `new → accepted | rejected`,
`accepted → completed | rejected` и повтор текущего статуса для правки комментария.
Условный UPDATE сверяет прочитанный статус: параллельное изменение не может
обойти проверку перехода. Статус, комментарий и `updated_at` обновляются атомарно,
`created_at` сохраняется. Автор и содержимое запроса после создания неизменяемы.
Обратная миграция 00008 не удаляет пользовательские запросы: секция Down пуста.

## Очередь и результаты проверки

Миграция `00006_judge_pipeline.sql` добавляет `cpu_time_ms` в `test_runs`,
`stop_on_first_failure` и `checker_ready` в `problems`, `compile_env` в `languages`.
Индекс очереди полный: частичный индекс только по `locked_by IS NULL` не покрывал
бы просроченные аренды. Claim увеличивает attempts и получает аренду на 60 секунд;
каждая запись проверяет владельца, номер попытки и срок аренды под блокировкой
строки задания. Поэтому упавший или потерявший аренду воркер не перезапишет результат.

Новая попытка очищает `test_runs`; отдельные результаты записываются через upsert.
После трёх системных ошибок задание остаётся для диагностики с `last_error` и
`run_after = infinity`, посылка получает `failed / SE`. При успехе задание удаляется
в одной транзакции с итогом посылки. При SIGTERM аренда снимается, attempts уменьшается.

## Уведомления и отчёт (HPRPR-25, HPRPR-26)

Схема не меняется, миграция `00007` не требуется. В транзакциях `Start`, `Running`,
`SaveTest`, `Finish`, `Fail`, `Release` воркер выполняет
`SELECT pg_notify('submission_events', '<submission UUID>')`. Сигнал доступен только
после commit и не содержит результатов или тестовых данных. Rollback отменяет и сигнал.

API держит одно отдельное от пула соединение с `LISTEN submission_events`.
По UUID перечитывает состояние, а после переподключения будит все локальные подписки.
NOTIFY может объединять события и не хранит историю: дополнительная сверка раз в
30 секунд восстанавливает текущее состояние при пропуске последнего сигнала.
Медленные клиенты не задерживают LISTEN и других подписчиков.

Отчёт читает `submissions.language_id/verdict/score/compile_log`, результаты
`test_runs` и признаки `test_cases.is_sample`. Для открытого теста с WA вход/эталон
читаются из S3 по `input_key/expected_key`, фактический вывод — по `stdout_key`.
Воркер уже сохраняет stdout только открытых тестов. Закрытые ключи не возвращаются
запросом результатов; дополнительно раскрытие запрещено при сборке отчёта.
`stdout_key IS NULL` у старого прогона означает отсутствие артефакта, а не пустой stdout.
Каждый показываемый файл ограничен 4 КиБ, diff строится по этим префиксам.
`report_key` остаётся резервом для экспортированных артефактов, JSON собирается по запросу.

## Биллинг и квоты (00009)

- `plans`: код (`free` / `pro`), название, цена `amount_kopecks bigint`, срок в днях,
  число компиляций, окно в секундах и месячный лимит прогонов. Нулевой лимит означает
  отсутствие ограничения. Free: 1 / 60 секунд и 100 прогонов; Pro: 29900 копеек / 30 дней.
- `subscriptions`: одна строка на пользователя, план, `active` / `canceled`,
  `starts_at` / `ends_at`. Просроченная или отсутствующая подписка даёт эффективный free;
  дата окончания проверяется при чтении, отдельный cron для истечения не нужен.
- `payments`: наш UUID, владелец, тариф, UUID запроса, уникальные `provider_id`
  и `idempotence_key`, снимок суммы/длительности, URL возврата и подтверждения,
  `pending` / `succeeded` / `canceled`, время следующей сверки. Пара `(user_id, request_id)`
  уникальна, чтобы клиентский повтор не создал новую покупку. Строка создаётся **до** API ЮKassa.
- `test_runs.created_at`: календарный месяц UTC по времени записи результата;
  индекс `(created_at, submission_id)` поддерживает подсчёт расхода. У старых результатов
  дата восстанавливается из `finished_at` / `started_at` / `created_at` посылки,
  чтобы история целиком не попала в месяц применения миграции.
- `submissions.test_run_budget`: число готовых тестов при приёме посылки. Активная
  очередь резервирует `max(budget - число сохранённых результатов, 0)` прогонов,
  чтобы параллельные отправки не обходили квоту. Завершённые `done` / `failed`
  посылки резерв не удерживают. Результаты предыдущей попытки воркер по-прежнему
  очищает; квота считает сохранённые `test_runs`, а не системные повторы проверки.

Операция приёма посылки блокирует строку `users FOR UPDATE`, проверяет прежний
`client_request_id`, выбирает эффективный тариф, считает компилируемые посылки
в скользящем окне и месячные прогоны с резервом; затем вставляет посылку и `judge_jobs`
в той же транзакции. Источником расхода остаётся БД, локальных счётчиков API нет.

Подтверждение оплаты блокирует `payments FOR UPDATE`, проверяет ответ API ЮKassa
и в одной транзакции переводит pending в succeeded и изменяет подписку.
Повторный webhook или фоновой GET не добавляет период ещё раз. Разные успешные
оплаты пользователя сериализуются через `users FOR UPDATE` и продлевают доступ
от `max(текущее окончание, now())`. Цена и длительность берутся из снимка покупки,
поэтому редактирование каталога не меняет уже оплаченные условия.

Фоновая сверка в процессе API берёт до 50 pending-платежей через `FOR UPDATE SKIP LOCKED`,
переносит `next_sync_at` на 30 секунд и читает провайдера вне транзакции.
Ошибка одного платежа не блокирует остальные. Для потерянного ответа создания POST
повторяется с сохранённым ключом в пределах 23 часов; более старое неопределённое
создание остаётся pending для ручной сверки, без риска повторного списания.
Миграция 00009 выполняется только вперёд, Down содержит поясняющий комментарий.

## Личное пространство и очередь запусков (00011)

Миграция `00011_playground.sql` добавляет три таблицы:

- `playground_projects`: UUIDv7 `id`, `user_id`, `title`, `language_id`, набор
  текстовых `files jsonb`, обязательный `client_request_id`, `created_at`,
  `updated_at`, `deleted_at`. Уникальность `(user_id, client_request_id)` действует
  и после удаления. Частичный индекс `(user_id, id DESC) WHERE deleted_at IS NULL`
  обслуживает список и подсчёт активных проектов. Мягкое удаление освобождает место
  в пространстве и сохраняет ссылки из запусков. Лимит 20 проектов проверяется
  под `users FOR UPDATE`; сервис проверяет размер в байтах, число и безопасные имена
  файлов, репозиторий дополнительно проверяет наличие source_file активного языка.
- `playground_runs`: UUIDv7, проект, владелец, язык, снимок доверенных команд языка
  `language jsonb`, `is_compiled`, файлы и stdin, ключ идемпотентности, статус
  `integer` (1 queued, 2 compiling, 3 running, 4 done, 5 failed), вердикт,
  код возврата, ограниченный вывод, время/память и даты создания/изменения.
  CHECK требует вердикт ровно в конечных состояниях. `is_compiled` фиксируется
  при приёме, поэтому редактирование проекта или каталога не меняет уже учтённый
  расход. Индекс `(user_id, created_at) WHERE is_compiled` обслуживает billing.
  Запуски, результаты и квота сохраняются при удалении проекта.
- `playground_jobs`: identity `bigint` PK, уникальный `run_id`, attempts,
  locked_by, locked_until, run_after, created_at, updated_at. Индекс
  `(run_after, id)` включает арендованные строки, чтобы находить просроченные аренды.

Приём запуска блокирует `users FOR UPDATE`, ищет прежний ключ до проверки квоты,
блокирует свой активный проект `FOR SHARE` и сохраняет снимок файлов/команд.
Проверка `billing.CheckCompilation` использует тот же `billing.Snapshot`, что
посылки: расход — UNION ALL компилируемых submissions и playground_runs в окне
эффективного тарифа. Вставка запуска и задания атомарна. Одновременная отправка
посылки и проекта сериализуется блокировкой одного пользователя. При повторе
ключа другого проекта возвращается конфликт; новый запуск удалённого проекта невозможен.
Месячная квота test_runs не расходуется и не резервируется.

Claim использует `FOR UPDATE SKIP LOCKED`, увеличивает attempts и выдаёт аренду 60s.
Heartbeat 20s, смена этапа, результат и повтор сверяют job id, run id, locked_by,
attempts и действующий locked_until под блокировкой строки. Поэтому результат
просроченной попытки не перезапишет новый. Результат фиксируется вместе с удалением
jobs; системный сбой после трёх попыток даёт failed/SE и тоже удаляет задание.
При штатной остановке attempts уменьшается, запуск возвращается в queued.
Промежуточные ошибки обрабатывает worker, исходные внутренние ошибки в API не раскрываются.
Миграция только вперёд, секция Down содержит запрет обратных миграций.
