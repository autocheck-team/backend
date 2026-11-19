package playground

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/autocheck-team/backend/internal/billing"
)

const runColumns = `id, project_id, user_id, client_request_id, language_id, language, files, stdin,
 status, verdict, exit_code, stdout, stderr, compile_log, is_output_truncated, time_ms, memory_kb, created_at, updated_at`

func scanRun(row pgx.Row) (Run, error) {
	var run Run
	err := row.Scan(&run.ID, &run.ProjectID, &run.UserID, &run.RequestID, &run.LanguageID,
		&run.Language, &run.Files, &run.Stdin, &run.Status, &run.Verdict, &run.ExitCode, &run.Stdout,
		&run.Stderr, &run.CompileLog, &run.Truncated, &run.TimeMS, &run.MemoryKB, &run.CreatedAt, &run.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, fmt.Errorf("read project run: %w", err)
	}
	return run, nil
}

// Enqueue фиксирует снимок, расход компиляции и очередь одной транзакцией.
func (r *Repository) Enqueue(ctx context.Context, projectID, userID, requestID uuid.UUID, stdin string) (Run, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	var result Run
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, userID); err != nil {
			return fmt.Errorf("lock run owner: %w", err)
		}
		existing, err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM playground_runs
   WHERE user_id=$1 AND client_request_id=$2`, userID, requestID))
		if err == nil {
			if existing.ProjectID != projectID {
				return ErrRequestConflict
			}
			result = existing
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		project, err := scanProject(tx.QueryRow(ctx, `SELECT `+projectColumns+` FROM playground_projects
   WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL FOR SHARE`, projectID, userID))
		if err != nil {
			return err
		}
		commands, err := language(ctx, tx, project.LanguageID, project.Files)
		if err != nil {
			return err
		}
		compiled := len(commands.CompileCmd) > 0
		if err := billing.CheckCompilation(ctx, tx, userID, compiled); err != nil {
			return err
		}
		result, err = scanRun(tx.QueryRow(ctx, `INSERT INTO playground_runs
   (project_id,user_id,client_request_id,language_id,language,is_compiled,files,stdin)
   VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+runColumns,
			projectID, userID, requestID, project.LanguageID, commands, compiled, project.Files, stdin))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO playground_jobs (run_id) VALUES ($1)`, result.ID); err != nil {
			return fmt.Errorf("enqueue project run: %w", err)
		}
		return nil
	})
	if err != nil {
		return Run{}, fmt.Errorf("accept project run: %w", err)
	}
	return result, nil
}

// GetRun читает неизменяемый снимок, включая результат после удаления проекта.
func (r *Repository) GetRun(ctx context.Context, id uuid.UUID) (Run, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return scanRun(r.pool.QueryRow(ctx, `SELECT `+runColumns+` FROM playground_runs WHERE id=$1`, id))
}

// Claim забирает свободное или просроченное задание с новым поколением аренды.
func (r *Repository) Claim(ctx context.Context, owner string) (Job, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	var job Job
	err := r.pool.QueryRow(ctx, `WITH ready AS (
 SELECT id FROM playground_jobs WHERE run_after<=now()
 AND (locked_by IS NULL OR locked_until<now()) ORDER BY run_after,id FOR UPDATE SKIP LOCKED LIMIT 1)
 UPDATE playground_jobs j SET attempts=attempts+1,locked_by=$1,
 locked_until=now()+interval '60 seconds',updated_at=now() FROM ready WHERE j.id=ready.id
 RETURNING j.id,j.run_id,j.locked_by,j.attempts`, owner).Scan(&job.ID, &job.RunID, &job.Owner, &job.Attempts)
	if err != nil {
		return Job{}, fmt.Errorf("claim playground job: %w", err)
	}
	return job, nil
}

func (r *Repository) lease(ctx context.Context, job Job, action func(context.Context, pgx.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var id int64
		err := tx.QueryRow(ctx, `SELECT id FROM playground_jobs WHERE id=$1 AND run_id=$2
   AND locked_by=$3 AND attempts=$4 AND locked_until>now() FOR UPDATE`,
			job.ID, job.RunID, job.Owner, job.Attempts).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return fmt.Errorf("check playground lease: %w", err)
		}
		if err := action(ctx, tx); err != nil {
			return fmt.Errorf("update leased playground run: %w", err)
		}
		return nil
	})
}

// Heartbeat продлевает только действующую аренду текущей попытки.
func (r *Repository) Heartbeat(ctx context.Context, job Job) error {
	return r.lease(ctx, job, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE playground_jobs SET locked_until=now()+interval '60 seconds',updated_at=now() WHERE id=$1`, job.ID)
		return err
	})
}

// Stage публикует этап только от владельца действующей аренды.
func (r *Repository) Stage(ctx context.Context, job Job, stage Status) error {
	return r.lease(ctx, job, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE playground_runs SET status=$2,updated_at=clock_timestamp() WHERE id=$1`, job.RunID, stage)
		return err
	})
}

// Finish сохраняет результат и удаляет задание атомарно.
func (r *Repository) Finish(ctx context.Context, job Job, result Result) error {
	return r.lease(ctx, job, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE playground_runs SET status=4,verdict=$2,exit_code=$3,
   stdout=$4,stderr=$5,compile_log=$6,is_output_truncated=$7,time_ms=$8,memory_kb=$9,
   updated_at=clock_timestamp() WHERE id=$1`, job.RunID, result.Verdict, result.ExitCode,
			result.Stdout, result.Stderr, result.CompileLog, result.Truncated, result.TimeMS, result.MemoryKB)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM playground_jobs WHERE id=$1`, job.ID)
		return err
	})
}

// Retry возвращает отменённую работу без расхода попытки либо ограничивает системные повторы.
func (r *Repository) Retry(ctx context.Context, job Job, release bool) error {
	return r.lease(ctx, job, func(ctx context.Context, tx pgx.Tx) error {
		if !release && job.Attempts >= 3 {
			if _, err := tx.Exec(ctx, `UPDATE playground_runs SET status=5,verdict='SE',updated_at=clock_timestamp() WHERE id=$1`, job.RunID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `DELETE FROM playground_jobs WHERE id=$1`, job.ID)
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE playground_runs SET status=1,updated_at=clock_timestamp() WHERE id=$1`, job.RunID); err != nil {
			return err
		}
		attempts := job.Attempts
		delay := time.Duration(1<<min(attempts, 3)) * time.Second
		if release {
			attempts--
			delay = 0
		}
		_, err := tx.Exec(ctx, `UPDATE playground_jobs SET attempts=$2,locked_by=NULL,locked_until=NULL,
   run_after=now()+$3::interval,updated_at=now() WHERE id=$1`, job.ID, attempts, delay.String())
		return err
	})
}
