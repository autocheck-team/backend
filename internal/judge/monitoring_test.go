package judge

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/autocheck-team/backend/internal/monitoring"
)

func TestQueueDepth(t *testing.T) {
	// arrange
	pool := judgeDB(t)
	repo := NewRepository(pool)
	userID, problemID := uuid.New(), uuid.New()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO users (id,email,display_name) VALUES ($1,$2,'queue metrics')`, userID, userID.String()+"@test.local")
	exec(`INSERT INTO problems (id,author_id,slug,title,statement,time_limit_ms,memory_limit_kb) VALUES ($1,$2,'metrics','Metrics','test',1000,65536)`, problemID, userID)
	for _, state := range []string{"waiting", "delayed", "running", "expired", "failed"} {
		subID := uuid.New()
		exec(`INSERT INTO submissions (id,problem_id,user_id,language_id,source_key,source_size) VALUES ($1,$2,$3,'python3.14','source',1)`, subID, problemID, userID)
		exec(`INSERT INTO judge_jobs (submission_id) VALUES ($1)`, subID)
		switch state {
		case "delayed":
			exec(`UPDATE judge_jobs SET run_after=now()+interval '1 minute' WHERE submission_id=$1`, subID)
		case "running":
			exec(`UPDATE judge_jobs SET locked_by='worker',locked_until=now()+interval '1 minute' WHERE submission_id=$1`, subID)
		case "expired":
			exec(`UPDATE judge_jobs SET locked_by='dead',locked_until=now()-interval '1 minute' WHERE submission_id=$1`, subID)
		case "failed":
			exec(`UPDATE submissions SET status='failed',verdict='SE' WHERE id=$1`, subID)
		}
	}
	// act
	waiting, running, err := repo.QueueDepth(t.Context())
	// assert
	if err != nil || waiting != 3 || running != 1 {
		t.Fatalf("waiting=%d running=%d err=%v", waiting, running, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	metrics := monitoring.New("worker")
	if err := ObserveQueue(ctx, repo, metrics, slog.New(slog.NewJSONHandler(io.Discard, nil)), time.Millisecond, time.Second); err != nil {
		t.Fatal(err)
	}
	assertMetric(t, metrics, `autocheck_judge_queue_depth{service="worker",state="waiting"} 3`)
}

func TestWorkerMetrics(t *testing.T) {
	for _, test := range []struct {
		name    string
		cause   error
		attempt int
		verdict string
		failure string
	}{
		{"success", nil, 1, "OK", "0"},
		{"retry", errors.New("docker unavailable"), 1, "", "1"},
		{"terminal", errors.New("docker unavailable"), 3, "SE", "1"},
		{"permanent", ErrNoTests, 1, "SE", "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// arrange
			metrics := monitoring.New("worker")
			worker := Worker{
				Queue: &fakeQueue{}, Log: slog.New(slog.NewJSONHandler(io.Discard, nil)), Metrics: metrics,
				Runner: executorFunc(func(context.Context, Job) (Outcome, error) { return Outcome{Verdict: "OK"}, test.cause }),
			}
			// act
			worker.process(t.Context(), Job{Attempts: test.attempt})
			// assert
			assertMetric(t, metrics, `autocheck_judge_active{service="worker"} 0`)
			assertMetric(t, metrics, `autocheck_judge_attempts_total{service="worker"} 1`)
			assertMetric(t, metrics, `autocheck_judge_system_errors_total{service="worker"} `+test.failure)
			if test.verdict != "" {
				assertMetric(t, metrics, `autocheck_judge_verdicts_total{service="worker",verdict="`+test.verdict+`"} 1`)
			}
		})
	}
}

func assertMetric(t *testing.T, metrics *monitoring.Metrics, line string) {
	t.Helper()
	response := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), "GET", "/metrics", nil))
	if !strings.Contains(response.Body.String(), line+"\n") {
		t.Fatalf("missing metric %s", line)
	}
}
