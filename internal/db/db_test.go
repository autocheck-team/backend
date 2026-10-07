package db

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Интеграционные тесты схемы. Нужна живая база: DATABASE_URL задаётся в CI
// и в docker-compose; без неё тесты пропускаются.
func setup(t *testing.T) (*pgxpool.Pool, pgx.Tx) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return pool, tx
}

func pgCode(err error) string {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pgErr.Code
	}
	return ""
}

// seed создаёт автора, задание и посылку и возвращает id посылки.
func seed(t *testing.T, tx pgx.Tx) string {
	t.Helper()
	ctx := context.Background()
	var userID, problemID, submissionID string
	err := tx.QueryRow(ctx, `INSERT INTO users (email, password_hash, display_name, role)
		VALUES ('Author@Example.com', 'x', 'Author', 'author') RETURNING id`).Scan(&userID)
	if err != nil {
		t.Fatal(err)
	}
	err = tx.QueryRow(ctx, `INSERT INTO problems (author_id, slug, title, statement, time_limit_ms, memory_limit_kb)
		VALUES ($1, 'a-plus-b', 'A+B', 'Сложите два числа', 1000, 262144) RETURNING id`, userID).Scan(&problemID)
	if err != nil {
		t.Fatal(err)
	}
	err = tx.QueryRow(ctx, `INSERT INTO submissions (problem_id, user_id, language_id, source_key, source_size)
		VALUES ($1, $2, 'go1.27', 'src/1', 42) RETURNING id`, problemID, userID).Scan(&submissionID)
	if err != nil {
		t.Fatal(err)
	}
	return submissionID
}

func TestEmailUniqueIgnoresCase(t *testing.T) {
	_, tx := setup(t)
	seed(t, tx)
	_, err := tx.Exec(context.Background(), `INSERT INTO users (email, password_hash, display_name)
		VALUES ('author@example.COM', 'x', 'Dup')`)
	if got := pgCode(err); got != "23505" {
		t.Fatalf("want unique_violation 23505, got %q (%v)", got, err)
	}
}

func TestVerdictRequiredWhenDone(t *testing.T) {
	_, tx := setup(t)
	id := seed(t, tx)
	// Ожидаемая ошибка ломает транзакцию, поэтому выполняем её в savepoint.
	sp, err := tx.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = sp.Exec(context.Background(), `UPDATE submissions SET status = 'done' WHERE id = $1`, id)
	if got := pgCode(err); got != "23514" {
		t.Fatalf("want check_violation 23514, got %q (%v)", got, err)
	}
	if err := sp.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(context.Background(), `UPDATE submissions SET status = 'done', verdict = 'OK' WHERE id = $1`, id)
	if err != nil {
		t.Fatal(err)
	}
}

func TestCustomCheckerNeedsKey(t *testing.T) {
	_, tx := setup(t)
	seed(t, tx)
	_, err := tx.Exec(context.Background(), `UPDATE problems SET checker = 'custom'`)
	if got := pgCode(err); got != "23514" {
		t.Fatalf("want check_violation 23514, got %q (%v)", got, err)
	}
}

// Две параллельные транзакции не должны забрать одно и то же задание.
func TestJudgeJobsSkipLocked(t *testing.T) {
	pool, tx := setup(t)
	ctx := context.Background()
	// Посылки нужны вне тестовой транзакции, чтобы их видела вторая транзакция.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	setupTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id := seed(t, setupTx)
	if _, err := setupTx.Exec(ctx, `INSERT INTO judge_jobs (submission_id) VALUES ($1)`, id); err != nil {
		t.Fatal(err)
	}
	if err := setupTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), `
			WITH s AS (DELETE FROM submissions WHERE id = $1 RETURNING problem_id, user_id),
			     p AS (DELETE FROM problems WHERE id IN (SELECT problem_id FROM s))
			DELETE FROM users WHERE id IN (SELECT user_id FROM s)`, id)
		if err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	const claim = `SELECT submission_id FROM judge_jobs
		WHERE locked_by IS NULL AND run_after <= now()
		ORDER BY run_after LIMIT 1 FOR UPDATE SKIP LOCKED`

	w1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w1.Rollback(ctx) }()
	var got string
	if err := w1.QueryRow(ctx, claim).Scan(&got); err != nil {
		t.Fatalf("worker 1: %v", err)
	}

	w2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w2.Rollback(ctx) }()
	if err := w2.QueryRow(ctx, claim).Scan(&got); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("worker 2: want no rows, got %v", err)
	}
}
