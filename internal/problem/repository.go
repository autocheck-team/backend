// Package problem — задания и их тесты: хранение в PostgreSQL и ProblemService.
package problem

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Ошибки репозитория.
var (
	ErrNotFound        = errors.New("not found")
	ErrSlugTaken       = errors.New("slug already taken")
	ErrUnknownLanguage = errors.New("unknown language")
	ErrHasSubmissions  = errors.New("problem has submissions")
)

// Problem — задание.
type Problem struct {
	ID             uuid.UUID
	AuthorID       uuid.UUID
	Slug           string
	Title          string
	Statement      string
	TimeLimitMS    int32
	MemoryLimitKB  int32
	Checker        string // значения типа checker_kind: exact, tokens, float, custom
	FloatEpsilon   *float64
	LanguageIDs    []string
	AllowAIHints   bool
	IsPublished    bool
	ReadyTestCount int32
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// TestCase — тест задания.
type TestCase struct {
	ID           uuid.UUID
	ProblemID    uuid.UUID
	Ordinal      int32
	IsSample     bool
	Points       int32
	IsReady      bool
	InputKey     string
	ExpectedKey  string
	InputSize    int64
	ExpectedSize int64
}

// Language — язык программирования.
type Language struct {
	ID   string
	Name string
}

// ListFilter — какие задания видит пользователь.
type ListFilter struct {
	// All — все задания (администратор).
	All bool
	// VisibleTo — кроме опубликованных видны неопубликованные этого автора.
	VisibleTo uuid.UUID
	// OnlyAuthor — только задания этого автора.
	OnlyAuthor uuid.UUID
	// After — id последнего задания предыдущей страницы (keyset-пагинация).
	After uuid.UUID
	Limit int
}

// Repository — задания и тесты в PostgreSQL.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository создаёт Repository.
func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

const problemColumns = `
	p.id, p.author_id, p.slug, p.title, p.statement, p.time_limit_ms, p.memory_limit_kb,
	p.checker::text, p.float_epsilon, p.allow_ai_hints, p.is_published, p.created_at, p.updated_at,
	coalesce((SELECT array_agg(pl.language_id ORDER BY pl.language_id)
	          FROM problem_languages pl WHERE pl.problem_id = p.id), '{}'),
	(SELECT count(*) FROM test_cases tc WHERE tc.problem_id = p.id AND tc.is_ready)::int`

func scanProblem(row pgx.Row) (Problem, error) {
	var p Problem
	err := row.Scan(&p.ID, &p.AuthorID, &p.Slug, &p.Title, &p.Statement, &p.TimeLimitMS, &p.MemoryLimitKB,
		&p.Checker, &p.FloatEpsilon, &p.AllowAIHints, &p.IsPublished, &p.CreatedAt, &p.UpdatedAt,
		&p.LanguageIDs, &p.ReadyTestCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return Problem{}, ErrNotFound
	}
	return p, err
}

// ListLanguages возвращает активные языки.
func (r *Repository) ListLanguages(ctx context.Context) ([]Language, error) {
	rows, err := r.pool.Query(ctx, `SELECT id, name FROM languages WHERE is_active ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list languages: %w", err)
	}
	langs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Language, error) {
		var l Language
		return l, row.Scan(&l.ID, &l.Name)
	})
	if err != nil {
		return nil, fmt.Errorf("list languages: %w", err)
	}
	return langs, nil
}

// Create создаёт задание вместе со списком языков.
func (r *Repository) Create(ctx context.Context, p Problem) (Problem, error) {
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO problems (id, author_id, slug, title, statement, time_limit_ms, memory_limit_kb,
			                      checker, float_epsilon, allow_ai_hints, is_published)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::checker_kind, $9, $10, $11)`,
			p.ID, p.AuthorID, p.Slug, p.Title, p.Statement, p.TimeLimitMS, p.MemoryLimitKB,
			p.Checker, p.FloatEpsilon, p.AllowAIHints, p.IsPublished)
		if err != nil {
			return err
		}
		return setLanguages(ctx, tx, p.ID, p.LanguageIDs)
	})
	if err != nil {
		return Problem{}, mapWriteError(err)
	}
	return r.Get(ctx, p.ID)
}

func setLanguages(ctx context.Context, tx pgx.Tx, problemID uuid.UUID, ids []string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM problem_languages WHERE problem_id = $1`, problemID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO problem_languages (problem_id, language_id)
		SELECT $1, l.id FROM unnest($2::text[]) AS u(id)
		LEFT JOIN languages l ON l.id = u.id AND l.is_active`, problemID, ids)
	// Неизвестный или выключенный язык даёт NULL в language_id: NOT NULL-ограничение
	// превращаем в ErrUnknownLanguage.
	return err
}

// mapWriteError переводит нарушения ограничений в ошибки репозитория.
func mapWriteError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch {
	case pgErr.Code == pgerrcode.UniqueViolation && pgErr.ConstraintName == "problems_slug_key":
		return ErrSlugTaken
	case pgErr.Code == pgerrcode.NotNullViolation && pgErr.ColumnName == "language_id",
		pgErr.Code == pgerrcode.ForeignKeyViolation && strings.Contains(pgErr.ConstraintName, "language"):
		return ErrUnknownLanguage
	}
	return err
}

// Get возвращает задание.
func (r *Repository) Get(ctx context.Context, id uuid.UUID) (Problem, error) {
	p, err := scanProblem(r.pool.QueryRow(ctx, `SELECT `+problemColumns+` FROM problems p WHERE p.id = $1`, id))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Problem{}, fmt.Errorf("get problem %s: %w", id, err)
	}
	return p, err
}

// List возвращает страницу заданий, новые первыми (uuidv7 упорядочен по времени).
func (r *Repository) List(ctx context.Context, f ListFilter) ([]Problem, error) {
	var (
		conds []string
		args  []any
	)
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if !f.All {
		conds = append(conds, "(p.is_published OR p.author_id = "+arg(f.VisibleTo)+")")
	}
	if f.OnlyAuthor != uuid.Nil {
		conds = append(conds, "p.author_id = "+arg(f.OnlyAuthor))
	}
	if f.After != uuid.Nil {
		conds = append(conds, "p.id < "+arg(f.After))
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	rows, err := r.pool.Query(ctx,
		`SELECT `+problemColumns+` FROM problems p`+where+` ORDER BY p.id DESC LIMIT `+arg(f.Limit), args...)
	if err != nil {
		return nil, fmt.Errorf("list problems: %w", err)
	}
	problems, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Problem, error) { return scanProblem(row) })
	if err != nil {
		return nil, fmt.Errorf("list problems: %w", err)
	}
	return problems, nil
}

// Update сохраняет все изменяемые поля задания. Какие поля менять, решает сервис.
func (r *Repository) Update(ctx context.Context, p Problem) (Problem, error) {
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE problems
			SET slug = $2, title = $3, statement = $4, time_limit_ms = $5, memory_limit_kb = $6,
			    checker = $7::checker_kind, float_epsilon = $8, allow_ai_hints = $9, is_published = $10,
			    updated_at = now()
			WHERE id = $1`,
			p.ID, p.Slug, p.Title, p.Statement, p.TimeLimitMS, p.MemoryLimitKB,
			p.Checker, p.FloatEpsilon, p.AllowAIHints, p.IsPublished)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return setLanguages(ctx, tx, p.ID, p.LanguageIDs)
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Problem{}, err
		}
		return Problem{}, mapWriteError(err)
	}
	return r.Get(ctx, p.ID)
}

// Delete удаляет задание и его тесты. Задание с посылками удалить нельзя.
func (r *Repository) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM problems p
		WHERE p.id = $1 AND NOT EXISTS (SELECT 1 FROM submissions s WHERE s.problem_id = p.id)`, id)
	if err != nil {
		return fmt.Errorf("delete problem %s: %w", id, err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	if _, err := r.Get(ctx, id); err != nil {
		return err
	}
	return ErrHasSubmissions
}

// CreateTestCase добавляет тест в конец списка тестов задания.
func (r *Repository) CreateTestCase(ctx context.Context, tc TestCase) (TestCase, error) {
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// Блокировка строки задания упорядочивает параллельные добавления тестов,
		// иначе два запроса получили бы одинаковый ordinal.
		var locked uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id FROM problems WHERE id = $1 FOR UPDATE`, tc.ProblemID).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO test_cases (id, problem_id, ordinal, input_key, expected_key, is_sample, points)
			VALUES ($1, $2, (SELECT coalesce(max(ordinal), 0) + 1 FROM test_cases WHERE problem_id = $2),
			        $3, $4, $5, $6)
			RETURNING ordinal`,
			tc.ID, tc.ProblemID, tc.InputKey, tc.ExpectedKey, tc.IsSample, tc.Points).Scan(&tc.Ordinal)
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return TestCase{}, err
		}
		return TestCase{}, fmt.Errorf("create test case: %w", err)
	}
	return tc, nil
}

const testCaseColumns = `id, problem_id, ordinal, is_sample, points, is_ready,
	input_key, expected_key, input_size, expected_size`

func scanTestCase(row pgx.Row) (TestCase, error) {
	var tc TestCase
	err := row.Scan(&tc.ID, &tc.ProblemID, &tc.Ordinal, &tc.IsSample, &tc.Points, &tc.IsReady,
		&tc.InputKey, &tc.ExpectedKey, &tc.InputSize, &tc.ExpectedSize)
	if errors.Is(err, pgx.ErrNoRows) {
		return TestCase{}, ErrNotFound
	}
	return tc, err
}

// GetTestCase возвращает тест задания.
func (r *Repository) GetTestCase(ctx context.Context, problemID, id uuid.UUID) (TestCase, error) {
	tc, err := scanTestCase(r.pool.QueryRow(ctx,
		`SELECT `+testCaseColumns+` FROM test_cases WHERE problem_id = $1 AND id = $2`, problemID, id))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return TestCase{}, fmt.Errorf("get test case %s: %w", id, err)
	}
	return tc, err
}

// MarkTestCaseReady отмечает, что файлы теста загружены.
func (r *Repository) MarkTestCaseReady(ctx context.Context, id uuid.UUID, inputSize, expectedSize int64) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE test_cases SET is_ready = true, input_size = $2, expected_size = $3 WHERE id = $1`,
		id, inputSize, expectedSize)
	if err != nil {
		return fmt.Errorf("mark test case %s ready: %w", id, err)
	}
	return nil
}

// ListTestCases возвращает тесты задания по порядку.
func (r *Repository) ListTestCases(ctx context.Context, problemID uuid.UUID, onlySamples bool) ([]TestCase, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+testCaseColumns+` FROM test_cases
		WHERE problem_id = $1 AND (NOT $2 OR (is_sample AND is_ready)) ORDER BY ordinal`, problemID, onlySamples)
	if err != nil {
		return nil, fmt.Errorf("list test cases: %w", err)
	}
	tcs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (TestCase, error) { return scanTestCase(row) })
	if err != nil {
		return nil, fmt.Errorf("list test cases: %w", err)
	}
	return tcs, nil
}

// DeleteTestCase удаляет тест. Результаты прогонов на нём удаляются каскадом.
func (r *Repository) DeleteTestCase(ctx context.Context, problemID, id uuid.UUID) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM test_cases WHERE problem_id = $1 AND id = $2`, problemID, id)
	if err != nil {
		return fmt.Errorf("delete test case %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
