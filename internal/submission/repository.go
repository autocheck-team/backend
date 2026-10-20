// Package submission — посылки: приём решений, постановка в очередь проверки и результаты.
package submission

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

// ErrNotFound — посылки или задания нет.
var ErrNotFound = errors.New("not found")

// errDuplicateRequest — посылка с таким client_request_id уже есть (гонка двух повторов).
var errDuplicateRequest = errors.New("duplicate client request")

// Submission — посылка.
type Submission struct {
	ID              uuid.UUID
	ProblemID       uuid.UUID
	ProblemAuthorID uuid.UUID
	UserID          uuid.UUID
	LanguageID      string
	SourceKey       string
	SourceSize      int32
	ClientRequestID *uuid.UUID
	Status          string  // значения submission_status
	Verdict         *string // значения verdict: OK, CE, WA, TL, ML, RE, SE
	Score           *int32
	MaxTimeMS       *int32
	MaxMemoryKB     *int32
	CompileLog      *string
	CreatedAt       time.Time
	StartedAt       *time.Time
	FinishedAt      *time.Time
}

// TestRun — результат на одном тесте.
type TestRun struct {
	TestCaseID uuid.UUID
	Ordinal    int32
	IsSample   bool
	Verdict    string
	TimeMS     int32
	MemoryKB   int32
}

// Target — то, что нужно знать о задании и языке для приёма посылки.
type Target struct {
	IsPublished     bool
	AuthorID        uuid.UUID
	LanguageAllowed bool
	// SourceFile — имя файла с исходником в песочнице, например "main.go".
	SourceFile string
}

// ListFilter — какие посылки вернуть.
type ListFilter struct {
	UserID    uuid.UUID // uuid.Nil — любого пользователя
	ProblemID uuid.UUID // uuid.Nil — любого задания
	After     uuid.UUID
	Limit     int
}

// Repository — посылки в PostgreSQL.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository создаёт Repository.
func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// Target возвращает сведения о задании и языке. ErrNotFound — задания нет.
func (r *Repository) Target(ctx context.Context, problemID uuid.UUID, languageID string) (Target, error) {
	var (
		t          Target
		sourceFile *string
	)
	err := r.pool.QueryRow(ctx, `
		SELECT p.is_published, p.author_id, l.source_file,
		       l.id IS NOT NULL AND EXISTS (
		           SELECT 1 FROM problem_languages pl WHERE pl.problem_id = p.id AND pl.language_id = l.id)
		FROM problems p
		LEFT JOIN languages l ON l.id = $2 AND l.is_active
		WHERE p.id = $1`, problemID, languageID).Scan(&t.IsPublished, &t.AuthorID, &sourceFile, &t.LanguageAllowed)
	if errors.Is(err, pgx.ErrNoRows) {
		return Target{}, ErrNotFound
	}
	if err != nil {
		return Target{}, fmt.Errorf("get submission target %s: %w", problemID, err)
	}
	if sourceFile != nil {
		t.SourceFile = *sourceFile
	}
	return t, nil
}

const submissionColumns = `
	s.id, s.problem_id, p.author_id, s.user_id, s.language_id, s.source_key, s.source_size,
	s.client_request_id, s.status::text, s.verdict::text, s.score, s.max_time_ms, s.max_memory_kb,
	s.compile_log, s.created_at, s.started_at, s.finished_at`

const submissionFrom = ` FROM submissions s JOIN problems p ON p.id = s.problem_id`

func scanSubmission(row pgx.Row) (Submission, error) {
	var s Submission
	err := row.Scan(&s.ID, &s.ProblemID, &s.ProblemAuthorID, &s.UserID, &s.LanguageID, &s.SourceKey, &s.SourceSize,
		&s.ClientRequestID, &s.Status, &s.Verdict, &s.Score, &s.MaxTimeMS, &s.MaxMemoryKB,
		&s.CompileLog, &s.CreatedAt, &s.StartedAt, &s.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Submission{}, ErrNotFound
	}
	return s, err
}

// Create сохраняет посылку и ставит её в очередь проверки одной транзакцией:
// посылка не может оказаться в базе без задания в очереди.
func (r *Repository) Create(ctx context.Context, s Submission) (Submission, error) {
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO submissions (id, problem_id, user_id, language_id, source_key, source_size, client_request_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			s.ID, s.ProblemID, s.UserID, s.LanguageID, s.SourceKey, s.SourceSize, s.ClientRequestID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO judge_jobs (submission_id) VALUES ($1)`, s.ID)
		return err
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation &&
			pgErr.ConstraintName == "submissions_client_request_key" {
			return Submission{}, errDuplicateRequest
		}
		return Submission{}, fmt.Errorf("create submission: %w", err)
	}
	return r.Get(ctx, s.ID)
}

// FindByClientRequest ищет посылку пользователя по ключу идемпотентности.
func (r *Repository) FindByClientRequest(ctx context.Context, userID, requestID uuid.UUID) (Submission, error) {
	s, err := scanSubmission(r.pool.QueryRow(ctx, `SELECT `+submissionColumns+submissionFrom+`
		WHERE s.user_id = $1 AND s.client_request_id = $2`, userID, requestID))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Submission{}, fmt.Errorf("find submission by client request: %w", err)
	}
	return s, err
}

// Get возвращает посылку.
func (r *Repository) Get(ctx context.Context, id uuid.UUID) (Submission, error) {
	s, err := scanSubmission(r.pool.QueryRow(ctx, `SELECT `+submissionColumns+submissionFrom+` WHERE s.id = $1`, id))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Submission{}, fmt.Errorf("get submission %s: %w", id, err)
	}
	return s, err
}

// List возвращает страницу посылок, новые первыми.
func (r *Repository) List(ctx context.Context, f ListFilter) ([]Submission, error) {
	var (
		conds []string
		args  []any
	)
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.UserID != uuid.Nil {
		conds = append(conds, "s.user_id = "+arg(f.UserID))
	}
	if f.ProblemID != uuid.Nil {
		conds = append(conds, "s.problem_id = "+arg(f.ProblemID))
	}
	if f.After != uuid.Nil {
		conds = append(conds, "s.id < "+arg(f.After))
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	rows, err := r.pool.Query(ctx,
		`SELECT `+submissionColumns+submissionFrom+where+` ORDER BY s.id DESC LIMIT `+arg(f.Limit), args...)
	if err != nil {
		return nil, fmt.Errorf("list submissions: %w", err)
	}
	subs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Submission, error) { return scanSubmission(row) })
	if err != nil {
		return nil, fmt.Errorf("list submissions: %w", err)
	}
	return subs, nil
}

// TestRuns возвращает результаты посылки по тестам в порядке тестов.
func (r *Repository) TestRuns(ctx context.Context, submissionID uuid.UUID) ([]TestRun, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT tr.test_case_id, tc.ordinal, tc.is_sample, tr.verdict::text, tr.time_ms, tr.memory_kb
		FROM test_runs tr JOIN test_cases tc ON tc.id = tr.test_case_id
		WHERE tr.submission_id = $1
		ORDER BY tc.ordinal`, submissionID)
	if err != nil {
		return nil, fmt.Errorf("list test runs: %w", err)
	}
	runs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (TestRun, error) {
		var t TestRun
		return t, row.Scan(&t.TestCaseID, &t.Ordinal, &t.IsSample, &t.Verdict, &t.TimeMS, &t.MemoryKB)
	})
	if err != nil {
		return nil, fmt.Errorf("list test runs: %w", err)
	}
	return runs, nil
}
