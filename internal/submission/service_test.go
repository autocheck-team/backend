package submission

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/autocheck-team/backend/internal/auth"
	"github.com/autocheck-team/backend/internal/db"
	"github.com/autocheck-team/backend/internal/storage"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

type memObjects struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (m *memObjects) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = b
	return nil
}

func (m *memObjects) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.data[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

type fixture struct {
	svc       *Service
	objects   *memObjects
	pool      *pgxpool.Pool
	student   auth.Principal
	student2  auth.Principal
	author    auth.Principal
	published uuid.UUID
	draft     uuid.UUID
}

func setup(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	f := &fixture{objects: &memObjects{data: map[string][]byte{}}, pool: pool}
	f.svc = NewService(NewRepository(pool), f.objects)
	mk := func(role auth.Role, roles ...auth.Role) auth.Principal {
		p := auth.Principal{ID: uuid.New(), Email: uuid.NewString() + "@test.local", Roles: roles}
		if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, display_name, role) VALUES ($1, $2, 'u', $3)`,
			p.ID, p.Email, string(role)); err != nil {
			t.Fatal(err)
		}
		return p
	}
	f.student = mk(auth.RoleStudent, auth.RoleStudent)
	f.student2 = mk(auth.RoleStudent, auth.RoleStudent)
	f.author = mk(auth.RoleAuthor, auth.RoleStudent, auth.RoleAuthor)

	mkProblem := func(published bool) uuid.UUID {
		id := uuid.New()
		slug := "s-" + strings.ReplaceAll(id.String(), "-", "")[:12]
		if _, err := pool.Exec(ctx, `INSERT INTO problems (id, author_id, slug, title, statement, time_limit_ms,
			memory_limit_kb, is_published) VALUES ($1, $2, $3, 't', 's', 1000, 65536, $4)`,
			id, f.author.ID, slug, published); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO problem_languages VALUES ($1, 'go1.27'), ($1, 'python3.14')`, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	f.published, f.draft = mkProblem(true), mkProblem(false)

	t.Cleanup(func() {
		ctx := context.Background()
		ids := []uuid.UUID{f.student.ID, f.student2.ID, f.author.ID}
		_, _ = pool.Exec(ctx, `DELETE FROM submissions WHERE user_id = ANY($1)`, ids)
		_, _ = pool.Exec(ctx, `DELETE FROM problems WHERE author_id = ANY($1)`, ids)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = ANY($1)`, ids)
	})
	return f
}

func as(p auth.Principal) context.Context { return auth.WithPrincipal(context.Background(), p) }

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("code = %s, want %s (err: %v)", got, want, err)
	}
}

func (f *fixture) submit(t *testing.T, who auth.Principal, problem uuid.UUID, lang, src, reqID string) *v1.Submission {
	t.Helper()
	resp, err := f.svc.CreateSubmission(as(who), &v1.CreateSubmissionRequest{
		ProblemId: problem.String(), LanguageId: lang, Source: src, ClientRequestId: reqID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp.GetSubmission()
}

func TestCreateSubmission(t *testing.T) {
	f := setup(t)
	const src = "package main\n\nfunc main() {}\n"

	sub := f.submit(t, f.student, f.published, "go1.27", src, "")
	if sub.GetStatus() != v1.SubmissionStatus_SUBMISSION_STATUS_QUEUED || sub.GetSourceSizeBytes() != int32(len(src)) ||
		sub.GetVerdict() != v1.Verdict_VERDICT_UNSPECIFIED {
		t.Fatalf("unexpected submission: %v", sub)
	}

	t.Run("source_stored_and_job_queued", func(t *testing.T) {
		if got := string(f.objects.data["submissions/"+sub.GetId()+"/main.go"]); got != src {
			t.Fatalf("stored source = %q", got)
		}
		var jobs int
		if err := f.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM judge_jobs WHERE submission_id = $1`, sub.GetId()).Scan(&jobs); err != nil || jobs != 1 {
			t.Fatalf("judge_jobs = %d, err %v", jobs, err)
		}
	})
	t.Run("language_not_allowed", func(t *testing.T) {
		_, err := f.svc.CreateSubmission(as(f.student), &v1.CreateSubmissionRequest{
			ProblemId: f.published.String(), LanguageId: "java25", Source: "class Main {}",
		})
		wantCode(t, err, codes.InvalidArgument)
	})
	t.Run("draft_hidden_from_students", func(t *testing.T) {
		_, err := f.svc.CreateSubmission(as(f.student), &v1.CreateSubmissionRequest{
			ProblemId: f.draft.String(), LanguageId: "go1.27", Source: src,
		})
		wantCode(t, err, codes.NotFound)
	})
	t.Run("author_can_submit_to_own_draft", func(t *testing.T) {
		f.submit(t, f.author, f.draft, "python3.14", "print(1)\n", "")
	})
	t.Run("unknown_problem", func(t *testing.T) {
		_, err := f.svc.CreateSubmission(as(f.student), &v1.CreateSubmissionRequest{
			ProblemId: uuid.NewString(), LanguageId: "go1.27", Source: src,
		})
		wantCode(t, err, codes.NotFound)
	})
}

func TestCreateSubmissionIdempotent(t *testing.T) {
	f := setup(t)
	reqID := uuid.NewString()

	first := f.submit(t, f.student, f.published, "go1.27", "package main", reqID)
	second := f.submit(t, f.student, f.published, "go1.27", "package main", reqID)
	if first.GetId() != second.GetId() {
		t.Fatalf("retry created a new submission: %s != %s", first.GetId(), second.GetId())
	}

	// Тот же ключ у другого пользователя — другая посылка.
	other := f.submit(t, f.student2, f.published, "go1.27", "package main", reqID)
	if other.GetId() == first.GetId() {
		t.Fatal("client_request_id must be scoped to user")
	}
}

func TestSubmissionVisibility(t *testing.T) {
	f := setup(t)
	sub := f.submit(t, f.student, f.published, "python3.14", "print(input())\n", "")
	f.submit(t, f.student2, f.published, "python3.14", "print(2)\n", "")

	t.Run("owner_reads_submission_and_source", func(t *testing.T) {
		if _, err := f.svc.GetSubmission(as(f.student), &v1.GetSubmissionRequest{Id: sub.GetId()}); err != nil {
			t.Fatal(err)
		}
		src, err := f.svc.GetSubmissionSource(as(f.student), &v1.GetSubmissionSourceRequest{Id: sub.GetId()})
		if err != nil {
			t.Fatal(err)
		}
		if src.GetSource() != "print(input())\n" || src.GetFileName() != "main.py" {
			t.Fatalf("source = %v", src)
		}
	})
	t.Run("other_student_does_not_see", func(t *testing.T) {
		_, err := f.svc.GetSubmission(as(f.student2), &v1.GetSubmissionRequest{Id: sub.GetId()})
		wantCode(t, err, codes.NotFound)
		_, err = f.svc.GetSubmissionSource(as(f.student2), &v1.GetSubmissionSourceRequest{Id: sub.GetId()})
		wantCode(t, err, codes.NotFound)
	})
	t.Run("problem_author_sees", func(t *testing.T) {
		if _, err := f.svc.GetSubmission(as(f.author), &v1.GetSubmissionRequest{Id: sub.GetId()}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("student_lists_only_own", func(t *testing.T) {
		resp, err := f.svc.ListSubmissions(as(f.student), &v1.ListSubmissionsRequest{ProblemId: f.published.String()})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.GetSubmissions()) != 1 || resp.GetSubmissions()[0].GetId() != sub.GetId() {
			t.Fatalf("student sees %d submissions", len(resp.GetSubmissions()))
		}
	})
	t.Run("author_lists_all_for_own_problem", func(t *testing.T) {
		resp, err := f.svc.ListSubmissions(as(f.author), &v1.ListSubmissionsRequest{ProblemId: f.published.String()})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.GetSubmissions()) != 2 {
			t.Fatalf("author sees %d submissions, want 2", len(resp.GetSubmissions()))
		}
	})
	t.Run("pagination", func(t *testing.T) {
		page1, err := f.svc.ListSubmissions(as(f.author), &v1.ListSubmissionsRequest{ProblemId: f.published.String(), PageSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		page2, err := f.svc.ListSubmissions(as(f.author), &v1.ListSubmissionsRequest{
			ProblemId: f.published.String(), PageSize: 1, PageToken: page1.GetNextPageToken(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if page1.GetNextPageToken() == "" || len(page2.GetSubmissions()) != 1 || page2.GetNextPageToken() != "" ||
			page1.GetSubmissions()[0].GetId() == page2.GetSubmissions()[0].GetId() {
			t.Fatalf("bad pagination: %v / %v", page1, page2)
		}
	})
}

func TestGetSubmissionWithTestRuns(t *testing.T) {
	f := setup(t)
	sub := f.submit(t, f.student, f.published, "go1.27", "package main", "")
	ctx := context.Background()

	// Результаты пишет воркер; здесь эмулируем завершённую проверку.
	var tcID uuid.UUID
	if err := f.pool.QueryRow(ctx, `INSERT INTO test_cases (id, problem_id, ordinal, input_key, expected_key, is_ready)
		VALUES (gen_random_uuid(), $1, 1, 'i', 'e', true) RETURNING id`, f.published).Scan(&tcID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO test_runs (submission_id, test_case_id, verdict, time_ms, memory_kb)
		VALUES ($1, $2, 'WA', 12, 2048)`, sub.GetId(), tcID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE submissions SET status = 'done', verdict = 'WA', score = 0,
		max_time_ms = 12, max_memory_kb = 2048, started_at = now(), finished_at = now() WHERE id = $1`, sub.GetId()); err != nil {
		t.Fatal(err)
	}

	resp, err := f.svc.GetSubmission(as(f.student), &v1.GetSubmissionRequest{Id: sub.GetId()})
	if err != nil {
		t.Fatal(err)
	}
	got := resp.GetSubmission()
	if got.GetStatus() != v1.SubmissionStatus_SUBMISSION_STATUS_DONE || got.GetVerdict() != v1.Verdict_VERDICT_WRONG_ANSWER ||
		got.GetMaxTimeMs() != 12 || got.GetFinishTime() == nil {
		t.Fatalf("unexpected submission: %v", got)
	}
	if len(resp.GetTestRuns()) != 1 || resp.GetTestRuns()[0].GetVerdict() != v1.Verdict_VERDICT_WRONG_ANSWER ||
		resp.GetTestRuns()[0].GetOrdinal() != 1 {
		t.Fatalf("unexpected test runs: %v", resp.GetTestRuns())
	}
}
