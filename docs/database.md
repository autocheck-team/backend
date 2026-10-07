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
```

Таблицы для платных функций (тарифы, подписки, счётчики лимитов) и ИИ-функций появятся
в своих задачах (HPRPR-12 и далее) отдельными миграциями.
