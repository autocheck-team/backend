package problem

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/autocheck-team/backend/internal/auth"
	"github.com/autocheck-team/backend/internal/db"
	"github.com/autocheck-team/backend/internal/storage"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

// fakeObjects — хранилище в памяти: «загрузка» — запись размера по ключу.
type fakeObjects struct {
	mu    sync.Mutex
	sizes map[string]int64
}

func (f *fakeObjects) PresignPut(_ context.Context, key string, _ time.Duration) (string, error) {
	return "http://s3.test/put/" + key, nil
}

func (f *fakeObjects) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "http://s3.test/get/" + key, nil
}

func (f *fakeObjects) Size(_ context.Context, key string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n, ok := f.sizes[key]; ok {
		return n, nil
	}
	return 0, storage.ErrNotFound
}

func (f *fakeObjects) DeletePrefix(_ context.Context, prefix string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.sizes {
		if strings.HasPrefix(k, prefix) {
			delete(f.sizes, k)
		}
	}
	return nil
}

func (f *fakeObjects) upload(key string, size int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sizes[key] = size
}

type fixture struct {
	svc     *Service
	objects *fakeObjects
	pool    *pgxpool.Pool
	student auth.Principal
	author  auth.Principal
	author2 auth.Principal
	admin   auth.Principal
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
	v, err := protovalidate.New()
	if err != nil {
		t.Fatal(err)
	}

	f := &fixture{objects: &fakeObjects{sizes: map[string]int64{}}, pool: pool}
	f.svc = NewService(NewRepository(pool), f.objects, v)
	mk := func(role auth.Role, roles ...auth.Role) auth.Principal {
		p := auth.Principal{ID: uuid.New(), Email: uuid.NewString() + "@test.local", Name: string(role), Roles: roles}
		_, err := pool.Exec(ctx, `INSERT INTO users (id, email, display_name, role) VALUES ($1, $2, $3, $4)`,
			p.ID, p.Email, p.Name, string(role))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	f.student = mk(auth.RoleStudent, auth.RoleStudent)
	f.author = mk(auth.RoleAuthor, auth.RoleStudent, auth.RoleAuthor)
	f.author2 = mk(auth.RoleAuthor, auth.RoleStudent, auth.RoleAuthor)
	f.admin = mk(auth.RoleAdmin, auth.RoleStudent, auth.RoleAuthor, auth.RoleAdmin)
	t.Cleanup(func() {
		ids := []uuid.UUID{f.student.ID, f.author.ID, f.author2.ID, f.admin.ID}
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM problems WHERE author_id = ANY($1)`, ids)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = ANY($1)`, ids)
	})
	return f
}

func as(p auth.Principal) context.Context {
	return auth.WithPrincipal(context.Background(), p)
}

func newProblem(slug string) *v1.Problem {
	return &v1.Problem{
		Slug: slug, Title: "A+B", Statement: "Сложите два числа",
		TimeLimitMs: 1000, MemoryLimitKb: 262144,
		Checker: v1.Checker_CHECKER_TOKENS, LanguageIds: []string{"go1.27", "python3.14"},
	}
}

func slug() string { return "p-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12] }

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("code = %s, want %s (err: %v)", got, want, err)
	}
}

func (f *fixture) create(t *testing.T, who auth.Principal) *v1.Problem {
	t.Helper()
	resp, err := f.svc.CreateProblem(as(who), &v1.CreateProblemRequest{Problem: newProblem(slug())})
	if err != nil {
		t.Fatal(err)
	}
	return resp.GetProblem()
}

// addReadyTest создаёт тест, «загружает» файлы и подтверждает его.
func (f *fixture) addReadyTest(t *testing.T, who auth.Principal, problemID string, sample bool) *v1.TestCase {
	t.Helper()
	ctx := as(who)
	created, err := f.svc.CreateTestCase(ctx, &v1.CreateTestCaseRequest{ProblemId: problemID, IsSample: sample})
	if err != nil {
		t.Fatal(err)
	}
	tc := created.GetTestCase()
	prefix := "problems/" + problemID + "/tests/" + tc.GetId()
	f.objects.upload(prefix+"/input", 4)
	f.objects.upload(prefix+"/expected", 2)
	confirmed, err := f.svc.ConfirmTestCase(ctx, &v1.ConfirmTestCaseRequest{ProblemId: problemID, Id: tc.GetId()})
	if err != nil {
		t.Fatal(err)
	}
	return confirmed.GetTestCase()
}

func TestCreateProblemPermissionsAndErrors(t *testing.T) {
	f := setup(t)

	t.Run("student_cannot_create", func(t *testing.T) {
		_, err := f.svc.CreateProblem(as(f.student), &v1.CreateProblemRequest{Problem: newProblem(slug())})
		wantCode(t, err, codes.PermissionDenied)
	})
	t.Run("author_creates_unpublished", func(t *testing.T) {
		p := f.create(t, f.author)
		if p.GetAuthorId() != f.author.ID.String() || p.GetIsPublished() || len(p.GetLanguageIds()) != 2 {
			t.Fatalf("unexpected problem: %v", p)
		}
	})
	t.Run("duplicate_slug", func(t *testing.T) {
		s := slug()
		if _, err := f.svc.CreateProblem(as(f.author), &v1.CreateProblemRequest{Problem: newProblem(s)}); err != nil {
			t.Fatal(err)
		}
		_, err := f.svc.CreateProblem(as(f.author2), &v1.CreateProblemRequest{Problem: newProblem(s)})
		wantCode(t, err, codes.AlreadyExists)
	})
	t.Run("unknown_language", func(t *testing.T) {
		p := newProblem(slug())
		p.LanguageIds = []string{"cobol"}
		_, err := f.svc.CreateProblem(as(f.author), &v1.CreateProblemRequest{Problem: p})
		wantCode(t, err, codes.InvalidArgument)
	})
	t.Run("cannot_publish_without_tests", func(t *testing.T) {
		p := newProblem(slug())
		p.IsPublished = true
		_, err := f.svc.CreateProblem(as(f.author), &v1.CreateProblemRequest{Problem: p})
		wantCode(t, err, codes.FailedPrecondition)
	})
	t.Run("custom_checker_not_supported", func(t *testing.T) {
		p := newProblem(slug())
		p.Checker = v1.Checker_CHECKER_CUSTOM
		_, err := f.svc.CreateProblem(as(f.author), &v1.CreateProblemRequest{Problem: p})
		wantCode(t, err, codes.Unimplemented)
	})
}

func TestVisibility(t *testing.T) {
	f := setup(t)
	p := f.create(t, f.author)

	for name, who := range map[string]auth.Principal{"student": f.student, "other_author": f.author2} {
		t.Run(name+"_does_not_see_unpublished", func(t *testing.T) {
			_, err := f.svc.GetProblem(as(who), &v1.GetProblemRequest{Id: p.GetId()})
			wantCode(t, err, codes.NotFound)
		})
	}
	t.Run("admin_sees_unpublished", func(t *testing.T) {
		if _, err := f.svc.GetProblem(as(f.admin), &v1.GetProblemRequest{Id: p.GetId()}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("other_author_cannot_edit", func(t *testing.T) {
		_, err := f.svc.CreateTestCase(as(f.author2), &v1.CreateTestCaseRequest{ProblemId: p.GetId()})
		wantCode(t, err, codes.NotFound)
	})
}

func TestTestCaseLifecycleAndPublishing(t *testing.T) {
	f := setup(t)
	p := f.create(t, f.author)
	ctx := as(f.author)

	created, err := f.svc.CreateTestCase(ctx, &v1.CreateTestCaseRequest{ProblemId: p.GetId(), IsSample: true})
	if err != nil {
		t.Fatal(err)
	}
	if created.GetInputUploadUrl() == "" || created.GetTestCase().GetOrdinal() != 1 || created.GetTestCase().GetIsReady() {
		t.Fatalf("unexpected create response: %v", created)
	}

	t.Run("confirm_before_upload_fails", func(t *testing.T) {
		_, err := f.svc.ConfirmTestCase(ctx, &v1.ConfirmTestCaseRequest{ProblemId: p.GetId(), Id: created.GetTestCase().GetId()})
		wantCode(t, err, codes.FailedPrecondition)
	})
	t.Run("publish_without_ready_tests_fails", func(t *testing.T) {
		_, err := f.svc.UpdateProblem(ctx, &v1.UpdateProblemRequest{
			Problem:    &v1.Problem{Id: p.GetId(), IsPublished: true},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"is_published"}},
		})
		wantCode(t, err, codes.FailedPrecondition)
	})

	sample := f.addReadyTest(t, f.author, p.GetId(), true)
	hidden := f.addReadyTest(t, f.author, p.GetId(), false)
	if sample.GetOrdinal() != 2 || hidden.GetOrdinal() != 3 || !hidden.GetIsReady() || hidden.GetInputSizeBytes() != 4 {
		t.Fatalf("unexpected tests: %v / %v", sample, hidden)
	}

	upd, err := f.svc.UpdateProblem(ctx, &v1.UpdateProblemRequest{
		Problem:    &v1.Problem{Id: p.GetId(), IsPublished: true},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"is_published"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !upd.GetProblem().GetIsPublished() || upd.GetProblem().GetReadyTestCount() != 2 {
		t.Fatalf("unexpected problem after publish: %v", upd.GetProblem())
	}

	t.Run("student_sees_published_and_only_ready_samples", func(t *testing.T) {
		if _, err := f.svc.GetProblem(as(f.student), &v1.GetProblemRequest{Id: p.GetId()}); err != nil {
			t.Fatal(err)
		}
		resp, err := f.svc.ListTestCases(as(f.student), &v1.ListTestCasesRequest{ProblemId: p.GetId()})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.GetTestCases()) != 1 || resp.GetTestCases()[0].GetId() != sample.GetId() ||
			resp.GetTestCases()[0].GetInputDownloadUrl() == "" {
			t.Fatalf("student tests = %v", resp.GetTestCases())
		}
	})
	t.Run("author_sees_all_tests", func(t *testing.T) {
		resp, err := f.svc.ListTestCases(ctx, &v1.ListTestCasesRequest{ProblemId: p.GetId()})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.GetTestCases()) != 3 {
			t.Fatalf("author tests = %d, want 3", len(resp.GetTestCases()))
		}
	})
	t.Run("delete_test_case_removes_files", func(t *testing.T) {
		_, err := f.svc.DeleteTestCase(ctx, &v1.DeleteTestCaseRequest{ProblemId: p.GetId(), Id: hidden.GetId()})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.objects.Size(ctx, "problems/"+p.GetId()+"/tests/"+hidden.GetId()+"/input"); err == nil {
			t.Fatal("test files were not deleted")
		}
	})
}

func TestUpdateProblemMask(t *testing.T) {
	f := setup(t)
	p := f.create(t, f.author)
	ctx := as(f.author)

	t.Run("partial_update_keeps_other_fields", func(t *testing.T) {
		resp, err := f.svc.UpdateProblem(ctx, &v1.UpdateProblemRequest{
			Problem:    &v1.Problem{Id: p.GetId(), Title: "Сумма", LanguageIds: []string{"cpp20-gcc15"}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"title", "language_ids"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		got := resp.GetProblem()
		want := proto.Clone(p).(*v1.Problem)
		want.Title, want.LanguageIds = "Сумма", []string{"cpp20-gcc15"}
		want.UpdateTime = got.GetUpdateTime()
		if !proto.Equal(got, want) {
			t.Fatalf("got %v\nwant %v", got, want)
		}
	})
	t.Run("readonly_field_rejected", func(t *testing.T) {
		_, err := f.svc.UpdateProblem(ctx, &v1.UpdateProblemRequest{
			Problem:    &v1.Problem{Id: p.GetId(), AuthorId: f.author2.ID.String()},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"author_id"}},
		})
		wantCode(t, err, codes.InvalidArgument)
	})
	t.Run("merged_problem_is_validated", func(t *testing.T) {
		_, err := f.svc.UpdateProblem(ctx, &v1.UpdateProblemRequest{
			Problem:    &v1.Problem{Id: p.GetId(), Checker: v1.Checker_CHECKER_FLOAT},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"checker"}},
		})
		wantCode(t, err, codes.InvalidArgument) // CHECKER_FLOAT без float_epsilon
	})
}

func TestListProblemsPagination(t *testing.T) {
	f := setup(t)
	for range 3 {
		f.create(t, f.author)
	}
	ctx := as(f.author)

	page1, err := f.svc.ListProblems(ctx, &v1.ListProblemsRequest{PageSize: 2, OnlyMine: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1.GetProblems()) != 2 || page1.GetNextPageToken() == "" {
		t.Fatalf("page1: %d problems, token %q", len(page1.GetProblems()), page1.GetNextPageToken())
	}
	page2, err := f.svc.ListProblems(ctx, &v1.ListProblemsRequest{PageSize: 2, OnlyMine: true, PageToken: page1.GetNextPageToken()})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2.GetProblems()) != 1 || page2.GetNextPageToken() != "" {
		t.Fatalf("page2: %d problems, token %q", len(page2.GetProblems()), page2.GetNextPageToken())
	}
	if page2.GetProblems()[0].GetId() >= page1.GetProblems()[1].GetId() {
		t.Fatal("pages are not ordered newest first")
	}

	t.Run("student_does_not_see_unpublished", func(t *testing.T) {
		resp, err := f.svc.ListProblems(as(f.student), &v1.ListProblemsRequest{PageSize: 100})
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range resp.GetProblems() {
			if !p.GetIsPublished() {
				t.Fatalf("student sees unpublished %s", p.GetId())
			}
		}
	})
}

func TestDeleteProblem(t *testing.T) {
	f := setup(t)

	t.Run("deletes_problem_and_files", func(t *testing.T) {
		p := f.create(t, f.author)
		f.addReadyTest(t, f.author, p.GetId(), false)
		if _, err := f.svc.DeleteProblem(as(f.author), &v1.DeleteProblemRequest{Id: p.GetId()}); err != nil {
			t.Fatal(err)
		}
		_, err := f.svc.GetProblem(as(f.author), &v1.GetProblemRequest{Id: p.GetId()})
		wantCode(t, err, codes.NotFound)
		if len(f.objects.sizes) != 0 {
			t.Fatalf("files left: %v", f.objects.sizes)
		}
	})
	t.Run("with_submissions_fails", func(t *testing.T) {
		p := f.create(t, f.author)
		_, err := f.pool.Exec(context.Background(), `INSERT INTO submissions (problem_id, user_id, language_id, source_key, source_size)
			VALUES ($1, $2, 'go1.27', 'x', 1)`, p.GetId(), f.student.ID)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = f.pool.Exec(context.Background(), `DELETE FROM submissions WHERE problem_id = $1`, p.GetId())
		})
		_, err = f.svc.DeleteProblem(as(f.author), &v1.DeleteProblemRequest{Id: p.GetId()})
		wantCode(t, err, codes.FailedPrecondition)
	})
}
