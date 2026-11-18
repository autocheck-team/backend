package submission

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/autocheck-team/backend/internal/auth"
	"github.com/autocheck-team/backend/internal/billing"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

func TestCompilationQuota(t *testing.T) {
	f := setup(t)
	f.submit(t, f.student, f.published, "go1.27", "package main", "")
	_, err := f.svc.CreateSubmission(as(f.student), &v1.CreateSubmissionRequest{ProblemId: f.published.String(), LanguageId: "go1.27", Source: "package main"})
	wantCode(t, err, codes.ResourceExhausted)
	var retry *errdetails.RetryInfo
	for _, detail := range status.Convert(err).Details() {
		if value, ok := detail.(*errdetails.RetryInfo); ok {
			retry = value
		}
	}
	if retry == nil || retry.GetRetryDelay().AsDuration() <= 0 || retry.GetRetryDelay().AsDuration() > 60*time.Second {
		t.Fatalf("retry=%v", retry)
	}
	// Интерпретируемые языки не тратят лимит компиляции.
	f.submit(t, f.student, f.published, "python3.14", "print(1)", "")
	if _, err := f.pool.Exec(t.Context(), `UPDATE submissions SET created_at=now()-interval '61 seconds' WHERE user_id=$1`, f.student.ID); err != nil {
		t.Fatal(err)
	}
	f.submit(t, f.student, f.published, "go1.27", "package main", "")
}

func TestCompilationQuotaConcurrent(t *testing.T) {
	f := setup(t)
	var group sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		group.Go(func() {
			_, err := f.svc.CreateSubmission(as(f.student), &v1.CreateSubmissionRequest{ProblemId: f.published.String(), LanguageId: "go1.27", Source: "package main"})
			errs <- err
		})
	}
	group.Wait()
	close(errs)
	accepted := 0
	for err := range errs {
		if err == nil {
			accepted++
		} else {
			wantCode(t, err, codes.ResourceExhausted)
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted=%d", accepted)
	}
}

func TestMonthlyQuota(t *testing.T) {
	for _, tc := range []struct {
		name     string
		used     int
		oldMonth bool
		pro      bool
		want     codes.Code
	}{
		{"last_run", 99, false, false, codes.OK},
		{"exhausted", 100, false, false, codes.ResourceExhausted},
		{"previous_month", 100, true, false, codes.OK},
		{"pro", 100, false, true, codes.OK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			var previous uuid.UUID
			if err := f.pool.QueryRow(t.Context(), `INSERT INTO submissions (problem_id,user_id,language_id,source_key,source_size,status,verdict)
    VALUES ($1,$2,'python3.14','source',1,'done','OK') RETURNING id`, f.published, f.student.ID).Scan(&previous); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pool.Exec(t.Context(), `INSERT INTO test_cases (problem_id,ordinal,input_key,expected_key,is_ready)
    SELECT $1,n,'in','expected',n=1 FROM generate_series(1,$2::integer) n`, f.published, tc.used); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pool.Exec(t.Context(), `INSERT INTO test_runs (submission_id,test_case_id,verdict,time_ms,memory_kb,created_at)
    SELECT $1,id,'OK',1,1,CASE WHEN $3 THEN date_trunc('month',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'-interval '1 second' ELSE now() END FROM test_cases WHERE problem_id=$2`, previous, f.published, tc.oldMonth); err != nil {
				t.Fatal(err)
			}
			if tc.pro {
				activatePro(t, f, f.student)
			}
			_, err := f.svc.CreateSubmission(as(f.student), &v1.CreateSubmissionRequest{ProblemId: f.published.String(), LanguageId: "python3.14", Source: "print(1)"})
			wantCode(t, err, tc.want)
			limits, err := billing.NewRepository(f.pool).Limits(t.Context(), f.student.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "last_run" && limits.RemainingTests() != 0 {
				t.Fatalf("reserved balance=%d", limits.RemainingTests())
			}
		})
	}
}

func activatePro(t *testing.T, f *fixture, user auth.Principal) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO subscriptions (user_id,plan_code,status,starts_at,ends_at)
  VALUES ($1,'pro','active',now(),now()+interval '30 days')`, user.ID); err != nil {
		t.Fatal(err)
	}
}

func TestQueuedTestsReserveQuota(t *testing.T) {
	f := setup(t)
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO test_cases (problem_id,ordinal,input_key,expected_key,is_ready)
  SELECT $1,n,'in','expected',true FROM generate_series(1,100) n`, f.published); err != nil {
		t.Fatal(err)
	}
	first := f.submit(t, f.student, f.published, "python3.14", "print(1)", uuid.NewString())
	_, err := f.svc.CreateSubmission(as(f.student), &v1.CreateSubmissionRequest{ProblemId: f.published.String(), LanguageId: "python3.14", Source: "print(2)"})
	wantCode(t, err, codes.ResourceExhausted)
	// CE не запускает тесты, завершённая посылка освобождает резерв.
	if _, err := f.pool.Exec(context.Background(), `UPDATE submissions SET status='done',verdict='CE' WHERE id=$1`, first.GetId()); err != nil {
		t.Fatal(err)
	}
	f.submit(t, f.student, f.published, "python3.14", "print(3)", "")
}

func TestProCompilationUnlimited(t *testing.T) {
	f := setup(t)
	activatePro(t, f, f.student)
	for range 3 {
		f.submit(t, f.student, f.published, "go1.27", "package main", "")
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE subscriptions SET starts_at=now()-interval '40 days',ends_at=now()-interval '1 second' WHERE user_id=$1`, f.student.ID); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.CreateSubmission(as(f.student), &v1.CreateSubmissionRequest{ProblemId: f.published.String(), LanguageId: "go1.27", Source: "package main"})
	wantCode(t, err, codes.ResourceExhausted)
}
