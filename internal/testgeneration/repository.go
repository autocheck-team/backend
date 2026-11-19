package testgeneration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/autocheck-team/backend/internal/billing"
)

// Ошибки доступа, конфликта и потерянной аренды не раскрывают содержимое эталона.
var (
	ErrNotFound  = errors.New("generation not found")
	ErrSelection = errors.New("invalid draft selection")
	ErrLeaseLost = errors.New("generation lease lost")
	ErrLanguage  = errors.New("language not enabled for problem")
)

const queryTimeout = 5 * time.Second

// Repository хранит отдельную очередь PostgreSQL с поколениями аренды.
type Repository struct{ pool *pgxpool.Pool }

// NewRepository создаёт хранилище без фоновых горутин.
func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// Author возвращает текущего владельца задания.
func (r *Repository) Author(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	var author uuid.UUID
	err := r.pool.QueryRow(ctx, `SELECT author_id FROM problems WHERE id=$1`, id).Scan(&author)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("read problem author: %w", err)
	}
	return author, nil
}

// Create сериализует квоту с платежами и посылками тем же замком users.
func (r *Repository) Create(ctx context.Context, g Generation, task Task, languageID string) (Generation, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var locked uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, g.UserID).Scan(&locked); err != nil {
			return fmt.Errorf("lock generation user: %w", err)
		}
		var previous uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id FROM test_generations WHERE user_id=$1 AND request_id=$2`, g.UserID, g.RequestID).Scan(&previous)
		if err == nil {
			g.ID = previous
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("find generation replay: %w", err)
		}
		if err := billing.CheckTestGeneration(ctx, tx, g.UserID); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT p.statement,p.time_limit_ms,p.memory_limit_kb,l.id,l.image,l.source_file,l.compile_cmd,l.run_cmd,l.compile_env
   FROM problems p JOIN problem_languages pl ON pl.problem_id=p.id JOIN languages l ON l.id=pl.language_id
   WHERE p.id=$1 AND l.id=$2 AND l.is_active`, g.ProblemID, languageID).Scan(
			&task.Statement, &task.TimeLimitMS, &task.MemoryLimitKB, &task.Language.ID, &task.Language.Image, &task.Language.SourceFile, &task.Language.CompileCmd, &task.Language.RunCmd, &task.Language.CompileEnv)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLanguage
		}
		if err != nil {
			return fmt.Errorf("snapshot generation task: %w", err)
		}
		data, err := json.Marshal(task)
		if err != nil {
			return fmt.Errorf("encode generation task: %w", err)
		}
		return tx.QueryRow(ctx, `INSERT INTO test_generations (problem_id,user_id,request_id,task) VALUES ($1,$2,$3,$4) RETURNING id`, g.ProblemID, g.UserID, g.RequestID, data).Scan(&g.ID)
	})
	if err != nil {
		return Generation{}, fmt.Errorf("enqueue test generation: %w", err)
	}
	result, err := r.Get(ctx, g.ProblemID, g.ID)
	if errors.Is(err, ErrNotFound) {
		return Generation{}, ErrSelection
	}
	return result, err
}

// Get не возвращает снимок исходника: он нужен только воркеру.
func (r *Repository) Get(ctx context.Context, problemID, id uuid.UUID) (Generation, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	var g Generation
	err := r.pool.QueryRow(ctx, `SELECT id,problem_id,user_id,request_id,status,error,created_at,updated_at FROM test_generations WHERE problem_id=$1 AND id=$2`, problemID, id).
		Scan(&g.ID, &g.ProblemID, &g.UserID, &g.RequestID, &g.Status, &g.Error, &g.CreatedAt, &g.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return g, ErrNotFound
	}
	if err != nil {
		return g, fmt.Errorf("read test generation: %w", err)
	}
	rows, err := r.pool.Query(ctx, `SELECT id,input,expected,explanation,rejection_reason,is_accepted FROM generated_tests WHERE generation_id=$1 ORDER BY ordinal`, id)
	if err != nil {
		return g, fmt.Errorf("list generated tests: %w", err)
	}
	g.Tests, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Draft, error) {
		var draft Draft
		err := row.Scan(&draft.ID, &draft.Input, &draft.Expected, &draft.Explanation, &draft.RejectionReason, &draft.IsAccepted)
		return draft, err
	})
	if err != nil {
		return g, fmt.Errorf("read generated tests: %w", err)
	}
	return g, nil
}

// Accept фиксирует выбранные тесты одной транзакцией после сохранения их файлов в S3.
func (r *Repository) Accept(ctx context.Context, g Generation, ids []uuid.UUID) error {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var locked uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM problems WHERE id=$1 FOR UPDATE`, g.ProblemID).Scan(&locked); err != nil {
			return fmt.Errorf("lock test problem: %w", err)
		}
		var state int32
		if err := tx.QueryRow(ctx, `SELECT status FROM test_generations WHERE id=$1 AND problem_id=$2 FOR UPDATE`, g.ID, g.ProblemID).Scan(&state); err != nil {
			return fmt.Errorf("lock generation: %w", err)
		}
		if state != 3 {
			return ErrSelection
		}
		rows, err := tx.Query(ctx, `SELECT id FROM generated_tests WHERE generation_id=$1 AND id=ANY($2::uuid[]) AND rejection_reason=''`, g.ID, ids)
		if err != nil {
			return fmt.Errorf("validate draft selection: %w", err)
		}
		selected, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (uuid.UUID, error) {
			var id uuid.UUID
			err := row.Scan(&id)
			return id, err
		})
		if err != nil {
			return fmt.Errorf("read draft selection: %w", err)
		}
		if len(selected) != len(ids) {
			return ErrSelection
		}
		_, err = tx.Exec(ctx, `INSERT INTO test_cases (id,problem_id,ordinal,input_key,expected_key,is_ready,is_generated,input_size,expected_size,points)
   SELECT d.id,$1,(SELECT COALESCE(max(ordinal),0) FROM test_cases WHERE problem_id=$1)+row_number() OVER (ORDER BY d.ordinal),
   'problems/'||$1::text||'/tests/'||d.id::text||'/input','problems/'||$1::text||'/tests/'||d.id::text||'/expected',true,true,octet_length(d.input),octet_length(d.expected),1
   FROM generated_tests d WHERE d.generation_id=$2 AND d.id=ANY($3::uuid[]) AND NOT d.is_accepted
   ORDER BY d.ordinal`, g.ProblemID, g.ID, ids)
		if err != nil {
			return fmt.Errorf("insert accepted tests: %w", err)
		}
		_, err = tx.Exec(ctx, `UPDATE generated_tests SET is_accepted=true,updated_at=now() WHERE generation_id=$1 AND id=ANY($2::uuid[])`, g.ID, ids)
		if err != nil {
			return fmt.Errorf("mark accepted drafts: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("accept generated tests: %w", err)
	}
	return nil
}
