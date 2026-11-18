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
    users ||--o{ refresh_tokens : ""
    users ||--o{ problems : "автор"
    users ||--o{ submissions : ""
    users ||--o{ feedback_requests : "автор запроса"
    languages |o--o{ feedback_requests : "необязательная привязка"
    problems ||--o{ test_cases : ""
    problems ||--o{ problem_languages : ""
    languages ||--o{ problem_languages : ""
    problems ||--o{ submissions : ""
    languages ||--o{ submissions : ""
    submissions ||--o{ test_runs : ""
    test_cases ||--o{ test_runs : ""
    submissions ||--o| judge_jobs : "в очереди"

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

Таблицы для платных функций (тарифы, подписки, счётчики лимитов) и ИИ-функций появятся
в своих задачах (HPRPR-12 и далее) отдельными миграциями.

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
