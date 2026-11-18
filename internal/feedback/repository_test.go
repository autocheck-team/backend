package feedback

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/autocheck-team/backend/internal/auth"
	"github.com/autocheck-team/backend/internal/db"
)

type fixture struct {
	pool    *pgxpool.Pool
	repo    *Repository
	svc     *Service
	student auth.Principal
	other   auth.Principal
	author  auth.Principal
	admin   auth.Principal
}

func setup(t *testing.T) *fixture {
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
	schema := "feedback_" + uuid.New().String()[:8]
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`); err != nil {
			t.Error(err)
		}
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
	f := &fixture{pool: pool, repo: NewRepository(pool, 5*time.Second)}
	f.svc = NewService(f.repo, testValidator(t))
	makeUser := func(role auth.Role) auth.Principal {
		t.Helper()
		user := auth.Principal{ID: uuid.New(), Roles: []auth.Role{role}}
		user.Email = user.ID.String() + "@test.local"
		if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, display_name, role)
			VALUES ($1, $2, 'feedback', $3)`, user.ID, user.Email, role); err != nil {
			t.Fatal(err)
		}
		return user
	}
	f.student, f.other = makeUser(auth.RoleStudent), makeUser(auth.RoleStudent)
	f.author, f.admin = makeUser(auth.RoleAuthor), makeUser(auth.RoleAdmin)
	return f
}

func (f *fixture) create(t *testing.T, user auth.Principal, kind Kind) Request {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	request, err := f.repo.Create(t.Context(), Request{
		ID: id, UserID: user.ID, Kind: kind, Title: "Заголовок", Text: "Текст запроса",
	})
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func TestRepositoryPersistence(t *testing.T) {
	f := setup(t)
	created := f.create(t, f.student, KindCompilerUpdate)
	if created.Status != StatusNew || created.LanguageID != nil || created.CreatedAt.IsZero() ||
		!created.CreatedAt.Equal(created.UpdatedAt) {
		t.Fatalf("defaults=%+v", created)
	}
	loaded, err := f.repo.Get(t.Context(), created.ID)
	if err != nil || loaded.ID != created.ID || loaded.Title != created.Title || loaded.Text != created.Text {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	updated, err := f.repo.Update(t.Context(), created.ID, StatusNew, StatusAccepted, "Берём в работу")
	if err != nil || updated.Status != StatusAccepted || updated.AdminComment != "Берём в работу" ||
		!updated.UpdatedAt.After(created.UpdatedAt) || !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	if _, err := f.repo.Update(t.Context(), created.ID, StatusNew, StatusRejected, "stale"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update=%v", err)
	}
	if _, err := f.repo.Get(t.Context(), uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing=%v", err)
	}
}

func TestRepositoryFilters(t *testing.T) {
	f := setup(t)
	first := f.create(t, f.student, KindCompilerUpdate)
	second := f.create(t, f.other, KindOther)
	third := f.create(t, f.student, KindOther)
	if _, err := f.repo.Update(t.Context(), third.ID, StatusNew, StatusAccepted, ""); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		filter ListFilter
		want   []uuid.UUID
	}{
		{"all", ListFilter{}, []uuid.UUID{third.ID, second.ID, first.ID}},
		{"own", ListFilter{UserID: f.student.ID}, []uuid.UUID{third.ID, first.ID}},
		{"status", ListFilter{Status: StatusAccepted}, []uuid.UUID{third.ID}},
		{"type", ListFilter{Kind: KindOther}, []uuid.UUID{third.ID, second.ID}},
		{"combined", ListFilter{UserID: f.student.ID, Status: StatusNew, Kind: KindCompilerUpdate}, []uuid.UUID{first.ID}},
		{"empty", ListFilter{Status: StatusCompleted}, nil},
		{"after", ListFilter{After: second.ID}, []uuid.UUID{first.ID}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.filter.Limit = 10
			requests, err := f.repo.List(t.Context(), tt.filter)
			if err != nil || len(requests) != len(tt.want) {
				t.Fatalf("len=%d want=%d err=%v", len(requests), len(tt.want), err)
			}
			for i, id := range tt.want {
				if requests[i].ID != id {
					t.Fatalf("row %d: %s want %s", i, requests[i].ID, id)
				}
			}
		})
	}
}

func TestRepositoryConstraints(t *testing.T) {
	f := setup(t)
	created := f.create(t, f.student, KindOther)
	for _, tt := range []struct {
		name  string
		sql   string
		value any
		code  string
	}{
		{"invalid_type", `UPDATE feedback_requests SET type=$2 WHERE id=$1`, 0, pgerrcode.CheckViolation},
		{"invalid_status", `UPDATE feedback_requests SET status=$2 WHERE id=$1`, 5, pgerrcode.CheckViolation},
		{"empty_title", `UPDATE feedback_requests SET title=$2 WHERE id=$1`, "", pgerrcode.CheckViolation},
		{"empty_text", `UPDATE feedback_requests SET text=$2 WHERE id=$1`, "", pgerrcode.CheckViolation},
		{"unknown_language", `UPDATE feedback_requests SET language_id=$2 WHERE id=$1`, "missing", pgerrcode.ForeignKeyViolation},
		{"unknown_user", `UPDATE feedback_requests SET user_id=$2 WHERE id=$1`, uuid.New(), pgerrcode.ForeignKeyViolation},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.pool.Exec(t.Context(), tt.sql, created.ID, tt.value)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != tt.code {
				t.Fatalf("err=%v", err)
			}
		})
	}
	var indexes int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_indexes
		WHERE schemaname=current_schema() AND tablename='feedback_requests'`).Scan(&indexes); err != nil || indexes != 9 {
		t.Fatalf("indexes=%d err=%v", indexes, err)
	}
}

func TestConcurrentIdempotency(t *testing.T) {
	f := setup(t)
	ctx := auth.WithPrincipal(t.Context(), f.student)
	requestID := uuid.NewString()
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			req := validCreate()
			req.ClientRequestId = requestID
			resp, err := f.svc.CreateFeedbackRequest(ctx, req)
			if err != nil {
				errs <- err
				return
			}
			ids <- resp.GetFeedbackRequest().GetId()
		})
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	id := ""
	for got := range ids {
		if id != "" && id != got {
			t.Fatalf("different ids: %s %s", id, got)
		}
		id = got
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM feedback_requests`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	req := validCreate()
	req.ClientRequestId, req.Title = requestID, "Изменённый заголовок"
	repeated, err := f.svc.CreateFeedbackRequest(ctx, req)
	if err != nil || repeated.GetFeedbackRequest().GetId() != id || repeated.GetFeedbackRequest().GetTitle() != validCreate().GetTitle() {
		t.Fatalf("repeat=%v err=%v", repeated, err)
	}
	other, err := f.svc.CreateFeedbackRequest(auth.WithPrincipal(t.Context(), f.other), req)
	if err != nil || other.GetFeedbackRequest().GetId() == id {
		t.Fatalf("other user=%v err=%v", other, err)
	}
}
