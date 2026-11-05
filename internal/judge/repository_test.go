package judge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/autocheck-team/backend/internal/db"
)

func judgeDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	admin, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := "judge_" + uuid.New().String()[:8]
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
	})
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestRepositoryPipeline(t *testing.T) {
	pool := judgeDB(t)
	r := NewRepository(pool)
	ctx := t.Context()
	user, problemID, subID, testID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO users (id,email,display_name) VALUES ($1,$2,'judge')`, user, user.String()+"@test.local")
	exec(`INSERT INTO problems (id,author_id,slug,title,statement,time_limit_ms,memory_limit_kb) VALUES ($1,$2,'judge','Judge','test',1000,65536)`, problemID, user)
	exec(`INSERT INTO submissions (id,problem_id,user_id,language_id,source_key,source_size) VALUES ($1,$2,$3,'python3.14','source',1)`, subID, problemID, user)
	exec(`INSERT INTO test_cases (id,problem_id,ordinal,input_key,expected_key,is_ready) VALUES ($1,$2,1,'in','exp',true)`, testID, problemID)
	exec(`INSERT INTO judge_jobs (submission_id) VALUES ($1)`, subID)
	var wg sync.WaitGroup
	jobs := make(chan Job, 2)
	errs := make(chan error, 2)
	for i := range 2 {
		wg.Go(func() {
			j, err := r.Claim(ctx, fmt.Sprintf("worker-%d", i))
			if err != nil {
				errs <- err
			} else {
				jobs <- j
			}
		})
	}
	wg.Wait()
	close(jobs)
	close(errs)
	if len(jobs) != 1 || len(errs) != 1 {
		t.Fatalf("claims=%d errors=%d", len(jobs), len(errs))
	}
	if err := <-errs; !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	old := <-jobs
	exec(`UPDATE judge_jobs SET locked_until=now()-interval '1 second' WHERE id=$1`, old.ID)
	next, err := r.Claim(ctx, "new-worker")
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != old.ID || next.Attempts != 2 {
		t.Fatalf("reclaim=%+v", next)
	}
	if err := r.Heartbeat(ctx, old); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old heartbeat=%v", err)
	}
	if err := r.Finish(ctx, old, Outcome{Verdict: "OK"}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old finish=%v", err)
	}
	if err := r.Release(ctx, next); err != nil {
		t.Fatal(err)
	}
	j, err := r.Claim(ctx, "runner")
	if err != nil {
		t.Fatal(err)
	}
	if j.Attempts != 2 {
		t.Fatalf("shutdown penalized attempts: %d", j.Attempts)
	}
	if err := r.Start(ctx, j); err != nil {
		t.Fatal(err)
	}
	tr := TestResult{TestID: testID, Verdict: "WA", StderrTail: "bad\x00\xff", CPUTimeMS: 7}
	for range 2 {
		if err := r.SaveTest(ctx, j, tr); err != nil {
			t.Fatal(err)
		}
	}
	var count, cpu int
	var tail string
	if err := pool.QueryRow(ctx, `SELECT count(*),max(cpu_time_ms),max(stderr_tail) FROM test_runs WHERE submission_id=$1`, subID).Scan(&count, &cpu, &tail); err != nil {
		t.Fatal(err)
	}
	if count != 1 || cpu != 7 || tail != "bad" {
		t.Fatalf("upsert: count=%d cpu=%d tail=%q", count, cpu, tail)
	}
	if err := r.Fail(ctx, j, errors.New("S3\x00\xff unavailable"), false); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE judge_jobs SET run_after=now() WHERE id=$1`, j.ID)
	j, err = r.Claim(ctx, "runner")
	if err != nil {
		t.Fatal(err)
	}
	if j.Attempts != 3 {
		t.Fatalf("attempt=%d", j.Attempts)
	}
	if err := r.Start(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM test_runs WHERE submission_id=$1`, subID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("retry clear: count=%d err=%v", count, err)
	}
	if err := r.Fail(ctx, j, errors.New("Docker unavailable"), false); err != nil {
		t.Fatal(err)
	}
	var state, verdict, lastError string
	if err := pool.QueryRow(ctx, `SELECT s.status::text,s.verdict::text,j.last_error FROM submissions s JOIN judge_jobs j ON j.submission_id=s.id WHERE s.id=$1`, subID).Scan(&state, &verdict, &lastError); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || verdict != "SE" || lastError != "Docker unavailable" {
		t.Fatalf("terminal=%s %s %q", state, verdict, lastError)
	}
	if _, err := r.Claim(ctx, "runner"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("claimed terminal: %v", err)
	}
	// Успешное завершение проверяется на той же посылке после явной перепроверки.
	exec(`UPDATE submissions SET status='queued',verdict=NULL WHERE id=$1`, subID)
	exec(`UPDATE judge_jobs SET attempts=0,run_after=now() WHERE submission_id=$1`, subID)
	j, err = r.Claim(ctx, "runner")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx, j, Outcome{Verdict: "OK", Score: 5, CompileLog: "ok\x00\xff"}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM judge_jobs WHERE submission_id=$1`, subID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("finish delete count=%d err=%v", count, err)
	}
}

func TestJudgeMigrationDownUp(t *testing.T) {
	pool := judgeDB(t)
	if err := db.Rollback(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='test_runs' AND column_name='cpu_time_ms')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("Down did not remove cpu_time_ms")
	}
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
}
