package playground

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/autocheck-team/backend/internal/auth"
	"github.com/autocheck-team/backend/internal/billing"
	"github.com/autocheck-team/backend/internal/submission"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

func (f *fixture) enqueue(t *testing.T, project *v1.Project) Run {
	t.Helper()
	run, err := f.repo.Enqueue(t.Context(), uuid.MustParse(project.Id), uuid.MustParse(project.UserId), uuid.New(), "input")
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestQueueSnapshotIdempotenceAndAccess(t *testing.T) {
	f := setup(t)
	project := f.create(t, f.student)
	owner := auth.WithPrincipal(t.Context(), f.student)
	request := &v1.RunProjectRequest{Id: project.Id, Stdin: "before", ClientRequestId: uuid.NewString()}
	var group sync.WaitGroup
	ids := make(chan string, 8)
	for range 8 {
		group.Go(func() {
			response, err := f.svc.RunProject(owner, request)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- response.Run.Id
		})
	}
	group.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if first != id {
			t.Fatal("duplicate runs")
		}
	}
	if first == "" {
		t.Fatal("no run accepted")
	}
	for _, principal := range []auth.Principal{f.student, f.other, f.author, f.admin} {
		code := codes.NotFound
		if principal.ID == f.student.ID || principal.ID == f.admin.ID {
			code = codes.OK
		}
		_, err := f.svc.GetProjectRun(auth.WithPrincipal(t.Context(), principal), &v1.GetProjectRunRequest{Id: first})
		wantCode(t, err, code)
	}
	updated, err := f.svc.UpdateProject(owner, &v1.UpdateProjectRequest{
		Id: project.Id, Title: "edited", LanguageId: "python3.14",
		Files: []*v1.ProjectFile{{Name: "main.py", Content: "print('after')"}},
	})
	if err != nil || !updated.Project.UpdateTime.AsTime().After(project.UpdateTime.AsTime()) {
		t.Fatal("update timestamp", err)
	}
	if err := f.repo.Delete(owner, uuid.MustParse(project.Id), f.student.ID); err != nil {
		t.Fatal(err)
	}
	// Повтор принятого запроса доступен и после удаления проекта.
	response, err := f.svc.RunProject(owner, request)
	if err != nil || response.Run.Id != first {
		t.Fatal("idempotence after delete", err)
	}
	saved, err := f.repo.GetRun(owner, uuid.MustParse(first))
	if err != nil || saved.Files[0].Content != "print('hello')" || saved.Stdin != "before" {
		t.Fatal("snapshot changed", err)
	}
	other := f.create(t, f.student)
	request.Id = other.Id
	_, err = f.svc.RunProject(owner, request)
	wantCode(t, err, codes.AlreadyExists)
	request.ClientRequestId = uuid.NewString()
	request.Stdin = "\x00"
	_, err = f.svc.RunProject(owner, request)
	wantCode(t, err, codes.InvalidArgument)
	request.Stdin = string(make([]byte, 65537))
	_, err = f.svc.RunProject(owner, request)
	wantCode(t, err, codes.InvalidArgument)
}

func TestQueueLeaseFencingAndRetry(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	run := f.enqueue(t, f.create(t, f.student))
	job, err := f.repo.Claim(ctx, "first")
	if err != nil || job.RunID != run.ID || job.Attempts != 1 {
		t.Fatal("claim", err)
	}
	if _, err := f.repo.Claim(ctx, "second"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("double claim", err)
	}
	if err := f.repo.Heartbeat(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.Stage(ctx, job, StatusRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE playground_jobs SET locked_until=now()-interval '1 second' WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.Finish(ctx, job, Result{Verdict: "OK"}); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("expired lease wrote result", err)
	}
	newer, err := f.repo.Claim(ctx, "second")
	if err != nil || newer.Attempts != 2 {
		t.Fatal("reclaim", err)
	}
	if err := f.repo.Heartbeat(ctx, job); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("old heartbeat", err)
	}
	if err := f.repo.Retry(ctx, job, false); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("old retry", err)
	}
	if err := f.repo.Retry(ctx, newer, true); err != nil {
		t.Fatal(err)
	}
	released, err := f.repo.Claim(ctx, "third")
	if err != nil || released.Attempts != 2 {
		t.Fatal("release consumed attempt", err)
	}
	if err := f.repo.Retry(ctx, released, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.Claim(ctx, "early"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("retry has no backoff", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE playground_jobs SET run_after=now() WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	final, err := f.repo.Claim(ctx, "last")
	if err != nil || final.Attempts != 3 {
		t.Fatal("third attempt", err)
	}
	if err := f.repo.Retry(ctx, final, false); err != nil {
		t.Fatal(err)
	}
	saved, err := f.repo.GetRun(ctx, run.ID)
	if err != nil || saved.Status != StatusFailed || saved.Verdict != "SE" {
		t.Fatal("failure not saved", err)
	}
	if _, err := f.repo.Claim(ctx, "after"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("failed job remained runnable", err)
	}
}

func TestQueueFinish(t *testing.T) {
	f := setup(t)
	run := f.enqueue(t, f.create(t, f.student))
	job, err := f.repo.Claim(t.Context(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	code := int32(2)
	if err := f.repo.Finish(t.Context(), job, Result{Verdict: "RE", ExitCode: &code, Stdout: "out", Stderr: "err", TimeMS: 12, MemoryKB: 42, Truncated: true}); err != nil {
		t.Fatal(err)
	}
	saved, err := f.repo.GetRun(t.Context(), run.ID)
	if err != nil || saved.Status != StatusDone || saved.Verdict != "RE" || saved.ExitCode == nil || *saved.ExitCode != 2 || saved.Stdout != "out" || saved.Stderr != "err" || saved.TimeMS != 12 || saved.MemoryKB != 42 || !saved.Truncated {
		t.Fatalf("result=%+v err=%v", saved.Result, err)
	}
	if err := f.repo.Finish(t.Context(), job, Result{Verdict: "OK"}); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("completed twice", err)
	}
}

func TestSharedCompilationQuota(t *testing.T) {
	for _, direction := range []string{"project_first", "submission_first"} {
		t.Run(direction, func(t *testing.T) {
			f := setup(t)
			ctx := t.Context()
			project, err := f.repo.Create(ctx, Project{UserID: f.student.ID, RequestID: uuid.New(), Title: "C++", LanguageID: "cpp20-gcc15", Files: []File{{Name: "main.cpp", Content: "int main(){}"}}})
			if err != nil {
				t.Fatal(err)
			}
			var problemID uuid.UUID
			if err := f.pool.QueryRow(ctx, `INSERT INTO problems (author_id,slug,title,statement,time_limit_ms,memory_limit_kb)
   VALUES ($1,'quota','quota','quota',1000,262144) RETURNING id`, f.author.ID).Scan(&problemID); err != nil {
				t.Fatal(err)
			}
			submit := func() error {
				_, err := submission.NewRepository(f.pool).Create(ctx, submission.Submission{ID: uuid.New(), UserID: f.student.ID, ProblemID: problemID, LanguageID: "cpp20-gcc15", SourceKey: "unused", SourceSize: 12})
				return err
			}
			enqueue := func() error { _, err := f.repo.Enqueue(ctx, project.ID, f.student.ID, uuid.New(), ""); return err }
			first, second := enqueue, submit
			if direction == "submission_first" {
				first, second = submit, enqueue
			}
			if err := first(); err != nil {
				t.Fatal(err)
			}
			var limit *billing.LimitError
			if err := second(); !errors.As(err, &limit) || limit.RetryAfter <= 0 {
				t.Fatal("quota not shared", err)
			}
			limits, err := billing.Snapshot(ctx, f.pool, f.student.ID)
			if err != nil || limits.Compilations != 1 || limits.Used != 0 || limits.Reserved != 0 {
				t.Fatalf("limits=%+v err=%v", limits, err)
			}
			// Pro отменяет ограничение для обеих очередей.
			if _, err := f.pool.Exec(ctx, `INSERT INTO subscriptions (user_id,plan_code,status,starts_at,ends_at)
   VALUES ($1,'pro','active',now()-interval '1 second',now()+interval '30 days')`, f.student.ID); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := enqueue(); err != nil {
					t.Fatal(err)
				}
				if err := submit(); err != nil {
					t.Fatal(err)
				}
			}
			// Удаление проекта не возвращает уже потраченную компиляцию.
			if err := f.repo.Delete(ctx, project.ID, f.student.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pool.Exec(ctx, `UPDATE subscriptions SET ends_at=now()-interval '1 second',starts_at=now()-interval '30 days' WHERE user_id=$1`, f.student.ID); err != nil {
				t.Fatal(err)
			}
			if err := submit(); !errors.As(err, &limit) {
				t.Fatal("delete refunded quota", err)
			}
			if _, err := f.pool.Exec(ctx, `UPDATE playground_runs SET created_at=now()-interval '61 seconds';UPDATE submissions SET created_at=now()-interval '61 seconds'`); err != nil {
				t.Fatal(err)
			}
			if err := submit(); err != nil {
				t.Fatal("quota window did not expire", err)
			}
		})
	}
}

func TestConcurrentCompilationQuota(t *testing.T) {
	f := setup(t)
	project, err := f.repo.Create(t.Context(), Project{UserID: f.student.ID, RequestID: uuid.New(), Title: "C++", LanguageID: "cpp20-gcc15", Files: []File{{Name: "main.cpp", Content: "int main(){}"}}})
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		group.Go(func() { _, err := f.repo.Enqueue(t.Context(), project.ID, f.student.ID, uuid.New(), ""); errs <- err })
	}
	group.Wait()
	close(errs)
	accepted := 0
	for err := range errs {
		if err == nil {
			accepted++
			continue
		}
		var limit *billing.LimitError
		if !errors.As(err, &limit) {
			t.Fatal(err)
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted=%d", accepted)
	}
}

func TestRepositoryTimeout(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.repo.List(ctx, f.student.ID, uuid.Nil, 1); err == nil {
		t.Fatal("cancelled query succeeded")
	}
	fast := NewRepository(f.pool, time.Nanosecond)
	if _, err := fast.GetRun(t.Context(), uuid.New()); err == nil {
		t.Fatal("timeout ignored")
	}
}

func TestCompiledRunRetryAndGatewayQuota(t *testing.T) {
	f := setup(t)
	g := startGateway(t, f)
	var created v1.CreateProjectResponse
	g.call(t, f.student, "POST", "/v1/projects", &v1.CreateProjectRequest{
		Title: "compile", LanguageId: "cpp20-gcc15", Files: []*v1.ProjectFile{{Name: "main.cpp", Content: "int main(){}"}},
		ClientRequestId: uuid.NewString(),
	}, &created, 200)
	path := "/v1/projects/" + created.Project.Id + ":run"
	request := &v1.RunProjectRequest{Id: created.Project.Id, ClientRequestId: uuid.NewString()}
	var first, repeated v1.RunProjectResponse
	g.call(t, f.student, "POST", path, request, &first, 200)
	g.call(t, f.student, "POST", path, request, &repeated, 200)
	if first.Run.Id != repeated.Run.Id {
		t.Fatal("retry consumed compilation")
	}
	request.ClientRequestId = uuid.NewString()
	g.call(t, f.student, "POST", path, request, nil, 429)
	ctx := auth.WithPrincipal(t.Context(), f.student)
	_, err := f.svc.RunProject(ctx, request)
	wantCode(t, err, codes.ResourceExhausted)
	var retry *errdetails.RetryInfo
	for _, detail := range status.Convert(err).Details() {
		if value, ok := detail.(*errdetails.RetryInfo); ok {
			retry = value
		}
	}
	if retry == nil || retry.RetryDelay.AsDuration() <= 0 {
		t.Fatal("missing RetryInfo")
	}
	// Интерпретируемые запуски не расходуют месячную квоту посылок или компиляции.
	if _, err := f.pool.Exec(t.Context(), `UPDATE plans SET monthly_test_run_limit=1 WHERE code='free'`); err != nil {
		t.Fatal(err)
	}
	python := f.create(t, f.student)
	f.enqueue(t, python)
	f.enqueue(t, python)
	limits, err := billing.Snapshot(t.Context(), f.pool, f.student.ID)
	if err != nil || limits.Compilations != 1 || limits.Used != 0 || limits.Reserved != 0 {
		t.Fatal("interpreted runs charged quota", err)
	}
}
