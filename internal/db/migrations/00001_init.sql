-- +goose Up

CREATE TYPE user_role AS ENUM ('student', 'author', 'admin');

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT uuidv7(),
    email         text        NOT NULL,
    password_hash text        NOT NULL,
    display_name  text        NOT NULL,
    role          user_role   NOT NULL DEFAULT 'student',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_email_key ON users (lower(email));

CREATE TABLE refresh_tokens (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash bytea       NOT NULL UNIQUE,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX refresh_tokens_user_idx ON refresh_tokens (user_id);

-- Языки и тулчейны. Образ и команды задаются данными, чтобы обновить
-- компилятор можно было без релиза бэкенда.
CREATE TABLE languages (
    id            text PRIMARY KEY,               -- 'go1.27', 'cpp20-gcc15', 'python3.14'
    name          text    NOT NULL,
    image         text    NOT NULL,               -- docker-образ песочницы
    source_file   text    NOT NULL,               -- имя файла с исходником внутри песочницы
    compile_cmd   text[],                         -- NULL для интерпретируемых языков
    run_cmd       text[]  NOT NULL,
    is_active     boolean NOT NULL DEFAULT true
);

CREATE TYPE checker_kind AS ENUM ('exact', 'tokens', 'float', 'custom');

CREATE TABLE problems (
    id              uuid PRIMARY KEY DEFAULT uuidv7(),
    author_id       uuid         NOT NULL REFERENCES users (id),
    slug            text         NOT NULL UNIQUE,
    title           text         NOT NULL,
    statement       text         NOT NULL,               -- markdown
    time_limit_ms   integer      NOT NULL CHECK (time_limit_ms BETWEEN 100 AND 30000),
    memory_limit_kb integer      NOT NULL CHECK (memory_limit_kb BETWEEN 4096 AND 2097152),
    checker         checker_kind NOT NULL DEFAULT 'tokens',
    float_epsilon   double precision,                    -- для checker = 'float'
    checker_key     text,                                -- объект в S3 для checker = 'custom'
    allow_ai_hints  boolean      NOT NULL DEFAULT false,
    is_published    boolean      NOT NULL DEFAULT false,
    created_at      timestamptz  NOT NULL DEFAULT now(),
    updated_at      timestamptz  NOT NULL DEFAULT now(),
    CHECK (checker <> 'float'  OR float_epsilon IS NOT NULL),
    CHECK (checker <> 'custom' OR checker_key IS NOT NULL)
);
CREATE INDEX problems_author_idx ON problems (author_id);

CREATE TABLE problem_languages (
    problem_id  uuid NOT NULL REFERENCES problems (id) ON DELETE CASCADE,
    language_id text NOT NULL REFERENCES languages (id),
    PRIMARY KEY (problem_id, language_id)
);

-- Входные и эталонные данные лежат в S3, в БД только ключи и метаданные.
CREATE TABLE test_cases (
    id           uuid PRIMARY KEY DEFAULT uuidv7(),
    problem_id   uuid    NOT NULL REFERENCES problems (id) ON DELETE CASCADE,
    ordinal      integer NOT NULL CHECK (ordinal > 0),
    input_key    text    NOT NULL,
    expected_key text    NOT NULL,
    is_sample    boolean NOT NULL DEFAULT false,  -- открытый тест: показываем вход и diff
    points       integer NOT NULL DEFAULT 1 CHECK (points >= 0),
    UNIQUE (problem_id, ordinal)
);

CREATE TYPE submission_status AS ENUM ('queued', 'compiling', 'running', 'done', 'failed');

-- OK — принято, CE — ошибка компиляции, WA — неверный ответ, TL — превышение
-- времени, ML — превышение памяти, RE — ошибка выполнения, SE — сбой проверяющей
-- системы (не вина студента).
CREATE TYPE verdict AS ENUM ('OK', 'CE', 'WA', 'TL', 'ML', 'RE', 'SE');

CREATE TABLE submissions (
    id            uuid PRIMARY KEY DEFAULT uuidv7(),
    problem_id    uuid              NOT NULL REFERENCES problems (id),
    user_id       uuid              NOT NULL REFERENCES users (id),
    language_id   text              NOT NULL REFERENCES languages (id),
    source_key    text              NOT NULL,
    source_size   integer           NOT NULL CHECK (source_size > 0),
    status        submission_status NOT NULL DEFAULT 'queued',
    verdict       verdict,
    score         integer,
    max_time_ms   integer,
    max_memory_kb integer,
    compile_log   text,
    report_key    text,                             -- экспортированный отчёт (PDF/JSON)
    created_at    timestamptz       NOT NULL DEFAULT now(),
    started_at    timestamptz,
    finished_at   timestamptz,
    CHECK ((status IN ('done', 'failed')) = (verdict IS NOT NULL))
);
CREATE INDEX submissions_user_idx    ON submissions (user_id, created_at DESC);
CREATE INDEX submissions_problem_idx ON submissions (problem_id, created_at DESC);

CREATE TABLE test_runs (
    submission_id uuid    NOT NULL REFERENCES submissions (id) ON DELETE CASCADE,
    test_case_id  uuid    NOT NULL REFERENCES test_cases (id) ON DELETE CASCADE,
    verdict       verdict NOT NULL,
    time_ms       integer NOT NULL,
    memory_kb     integer NOT NULL,
    exit_code     integer,
    stdout_key    text,     -- сохраняем только для открытых тестов, чтобы показать diff
    stderr_tail   text,     -- последние строки stderr, обрезанные воркером
    PRIMARY KEY (submission_id, test_case_id)
);

-- Очередь проверок поверх Postgres: воркеры забирают задания через
-- SELECT ... FOR UPDATE SKIP LOCKED, отдельный брокер не нужен.
CREATE TABLE judge_jobs (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    submission_id uuid        NOT NULL UNIQUE REFERENCES submissions (id) ON DELETE CASCADE,
    run_after     timestamptz NOT NULL DEFAULT now(),
    attempts      integer     NOT NULL DEFAULT 0,
    locked_by     text,
    locked_until  timestamptz,
    last_error    text
);
CREATE INDEX judge_jobs_ready_idx ON judge_jobs (run_after) WHERE locked_by IS NULL;

INSERT INTO languages (id, name, image, source_file, compile_cmd, run_cmd) VALUES
    ('go1.27',      'Go 1.27',          'autocheck/sandbox-go:1.27',     'main.go',
        ARRAY['go', 'build', '-o', 'solution', 'main.go'],       ARRAY['./solution']),
    ('cpp20-gcc15', 'C++20 (GCC 15)',   'autocheck/sandbox-gcc:15',      'main.cpp',
        ARRAY['g++', '-std=c++20', '-O2', '-o', 'solution', 'main.cpp'], ARRAY['./solution']),
    ('python3.14',  'Python 3.14',      'autocheck/sandbox-python:3.14', 'main.py',
        NULL,                                                    ARRAY['python3', 'main.py']),
    ('java25',      'Java 25',          'autocheck/sandbox-java:25',     'Main.java',
        ARRAY['javac', 'Main.java'],                             ARRAY['java', '-Xss64m', 'Main']);

-- +goose Down

DROP TABLE judge_jobs;
DROP TABLE test_runs;
DROP TABLE submissions;
DROP TYPE verdict;
DROP TYPE submission_status;
DROP TABLE test_cases;
DROP TABLE problem_languages;
DROP TABLE problems;
DROP TYPE checker_kind;
DROP TABLE languages;
DROP TABLE refresh_tokens;
DROP TABLE users;
DROP TYPE user_role;
