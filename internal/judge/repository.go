package judge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/autocheck-team/backend/internal/problem"
)

// ErrLeaseLost запрещает старой попытке записывать результаты после перехвата задания.
var ErrLeaseLost = errors.New("job lease lost")

const (
	leaseDuration   = 60 * time.Second
	databaseTimeout = 5 * time.Second
)

// Job содержит поколение аренды: даже повторяющийся WORKER_ID не даёт старому запуску права записи.
type Job struct {
	ID           int64
	SubmissionID uuid.UUID
	Owner        string
	Attempts     int
}

// Language хранит команды из БД, чтобы смена тулчейна не требовала релиза воркера.
type Language struct {
	ID, Image, SourceFile          string
	CompileCmd, RunCmd, CompileEnv []string
}

// Task — снимок задания и списка готовых тестов на начало попытки.
type Task struct {
	SourceKey string
	Problem   problem.Problem
	Language  Language
	Tests     []problem.TestCase
}

// TestResult содержит измерения и артефакты одного прогона.
type TestResult struct {
	TestID                      uuid.UUID
	Verdict                     string
	TimeMS, CPUTimeMS, MemoryKB int32
	ExitCode                    int
	StderrTail                  string
	StdoutKey                   *string
}

// Outcome записывается вместе с удалением задания, чтобы успешная проверка не повторилась.
type Outcome struct {
	Verdict                       string
	Score, MaxTimeMS, MaxMemoryKB int32
	CompileLog                    string
}

// Repository реализует очередь на PostgreSQL без внешнего брокера.
type Repository struct{ pool *pgxpool.Pool }

// NewRepository создаёт очередь.
func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// Claim захватывает также просроченные аренды; полный индекс не скрывает их из выборки.
func (r *Repository) Claim(ctx context.Context, owner string) (Job, error) {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	var j Job
	err := r.pool.QueryRow(ctx, `WITH ready AS (
 SELECT j.id FROM judge_jobs j JOIN submissions s ON s.id = j.submission_id
 WHERE j.run_after <= now() AND (j.locked_by IS NULL OR j.locked_until < now())
 AND s.status NOT IN ('done', 'failed')
 ORDER BY j.run_after, j.id FOR UPDATE OF j SKIP LOCKED LIMIT 1)
 UPDATE judge_jobs j SET attempts = attempts + 1, locked_by = $1,
 locked_until = now() + $2::interval FROM ready WHERE j.id = ready.id
 RETURNING j.id, j.submission_id, j.attempts`, owner, leaseDuration.String()).Scan(&j.ID, &j.SubmissionID, &j.Attempts)
	if err != nil {
		return Job{}, fmt.Errorf("claim job: %w", err)
	}
	j.Owner = owner
	return j, nil
}

// Heartbeat не оживляет уже истёкшую аренду, даже если её пока никто не забрал.
func (r *Repository) Heartbeat(ctx context.Context, j Job) error {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	tag, err := r.pool.Exec(ctx, `UPDATE judge_jobs SET locked_until = now() + $4::interval
 WHERE id = $1 AND locked_by = $2 AND attempts = $3 AND locked_until > now()`, j.ID, j.Owner, j.Attempts, leaseDuration.String())
	if err != nil {
		return fmt.Errorf("extend lease: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// owned блокирует аренду до commit: Claim и Heartbeat не могут обогнать запись результата.
func (r *Repository) owned(ctx context.Context, j Job, fn func(context.Context, pgx.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var id int64
		err := tx.QueryRow(ctx, `SELECT id FROM judge_jobs WHERE id = $1 AND locked_by = $2
   AND attempts = $3 AND locked_until > now() FOR UPDATE`, j.ID, j.Owner, j.Attempts).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return fmt.Errorf("lock owned job: %w", err)
		}
		return fn(ctx, tx)
	})
}

// Start стирает незавершённые результаты предыдущей попытки.
func (r *Repository) Start(ctx context.Context, j Job) error {
	return r.owned(ctx, j, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM test_runs WHERE submission_id = $1`, j.SubmissionID); err != nil {
			return fmt.Errorf("clear test runs: %w", err)
		}
		_, err := tx.Exec(ctx, `UPDATE submissions SET status = 'compiling', verdict = NULL, started_at = now(),
   finished_at = NULL, compile_log = NULL, score = NULL, max_time_ms = NULL, max_memory_kb = NULL WHERE id = $1`, j.SubmissionID)
		return err
	})
}

// Running меняет этап проверки после успешной компиляции.
func (r *Repository) Running(ctx context.Context, j Job) error {
	return r.owned(ctx, j, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE submissions SET status = 'running' WHERE id = $1`, j.SubmissionID)
		return err
	})
}

// SaveTest идемпотентен внутри попытки и не принимает результаты от потерявшего аренду воркера.
func (r *Repository) SaveTest(ctx context.Context, j Job, t TestResult) error {
	return r.owned(ctx, j, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO test_runs
   (submission_id, test_case_id, verdict, time_ms, cpu_time_ms, memory_kb, exit_code, stderr_tail, stdout_key)
   VALUES ($1,$2,$3::verdict,$4,$5,$6,$7,$8,$9)
   ON CONFLICT (submission_id, test_case_id) DO UPDATE SET verdict = excluded.verdict,
   time_ms = excluded.time_ms, cpu_time_ms = excluded.cpu_time_ms, memory_kb = excluded.memory_kb,
   exit_code = excluded.exit_code, stderr_tail = excluded.stderr_tail, stdout_key = excluded.stdout_key`,
			j.SubmissionID, t.TestID, t.Verdict, t.TimeMS, t.CPUTimeMS, t.MemoryKB, t.ExitCode,
			cleanTail([]byte(t.StderrTail), 4096), t.StdoutKey)
		return err
	})
}

// Finish атомарно публикует результат и убирает успешно обработанное задание.
func (r *Repository) Finish(ctx context.Context, j Job, o Outcome) error {
	return r.owned(ctx, j, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE submissions SET status = 'done', verdict = $2::verdict, score = $3,
   max_time_ms = $4, max_memory_kb = $5, compile_log = $6, finished_at = now() WHERE id = $1`,
			j.SubmissionID, o.Verdict, o.Score, o.MaxTimeMS, o.MaxMemoryKB, cleanTail([]byte(o.CompileLog), 64<<10))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM judge_jobs WHERE id = $1`, j.ID)
		return err
	})
}

// Fail оставляет last_error и даёт всего три попытки; отсутствие тестов завершается сразу.
func (r *Repository) Fail(ctx context.Context, j Job, cause error, permanent bool) error {
	return r.owned(ctx, j, func(ctx context.Context, tx pgx.Tx) error {
		terminal := permanent || j.Attempts >= 3
		status := "queued"
		var verdict *string
		if terminal {
			status = "failed"
			v := "SE"
			verdict = &v
		}
		_, err := tx.Exec(ctx, `UPDATE submissions SET status = $2::submission_status, verdict = $3::verdict,
   finished_at = CASE WHEN $4 THEN now() ELSE NULL END WHERE id = $1`, j.SubmissionID, status, verdict, terminal)
		if err != nil {
			return err
		}
		delay := time.Second * time.Duration(1<<min(j.Attempts, 10))
		_, err = tx.Exec(ctx, `UPDATE judge_jobs SET locked_by = NULL, locked_until = NULL, last_error = $2,
   run_after = CASE WHEN $3 THEN 'infinity'::timestamptz ELSE now() + $4::interval END WHERE id = $1`,
			j.ID, cleanTail([]byte(cause.Error()), 64<<10), terminal, delay.String())
		return err
	})
}

// Release возвращает попытку при SIGTERM: остановка сервиса не штрафует решение.
func (r *Repository) Release(ctx context.Context, j Job) error {
	return r.owned(ctx, j, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE submissions SET status = 'queued', verdict = NULL WHERE id = $1`, j.SubmissionID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE judge_jobs SET locked_by = NULL, locked_until = NULL,
   attempts = greatest(attempts - 1, 0), run_after = now() WHERE id = $1`, j.ID)
		return err
	})
}

// Load читает только готовые тесты, в порядке ordinal.
func (r *Repository) Load(ctx context.Context, j Job) (Task, error) {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	var t Task
	err := r.pool.QueryRow(ctx, `SELECT s.source_key, p.id, p.time_limit_ms, p.memory_limit_kb,
 p.checker::text, p.float_epsilon, p.checker_key, p.stop_on_first_failure,
 l.id, l.image, l.source_file, l.compile_cmd, l.run_cmd, l.compile_env
 FROM submissions s JOIN problems p ON p.id = s.problem_id JOIN languages l ON l.id = s.language_id
 WHERE s.id = $1`, j.SubmissionID).Scan(&t.SourceKey, &t.Problem.ID, &t.Problem.TimeLimitMS, &t.Problem.MemoryLimitKB,
		&t.Problem.Checker, &t.Problem.FloatEpsilon, &t.Problem.CheckerKey, &t.Problem.StopOnFirstFailure,
		&t.Language.ID, &t.Language.Image, &t.Language.SourceFile, &t.Language.CompileCmd, &t.Language.RunCmd, &t.Language.CompileEnv)
	if err != nil {
		return Task{}, fmt.Errorf("load judge task: %w", err)
	}
	rows, err := r.pool.Query(ctx, `SELECT id, ordinal, input_key, expected_key, is_sample, points
 FROM test_cases WHERE problem_id = $1 AND is_ready ORDER BY ordinal`, t.Problem.ID)
	if err != nil {
		return Task{}, fmt.Errorf("load ready tests: %w", err)
	}
	t.Tests, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (problem.TestCase, error) {
		var tc problem.TestCase
		err := row.Scan(&tc.ID, &tc.Ordinal, &tc.InputKey, &tc.ExpectedKey, &tc.IsSample, &tc.Points)
		return tc, err
	})
	if err != nil {
		return Task{}, fmt.Errorf("read ready tests: %w", err)
	}
	return t, nil
}
