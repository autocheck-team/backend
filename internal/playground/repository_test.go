package playground

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	schema := "playground_" + uuid.New().String()[:8]
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
	f.svc = New(f.repo, testValidator(t))
	makeUser := func(role auth.Role) auth.Principal {
		t.Helper()
		user := auth.Principal{ID: uuid.New(), Roles: []auth.Role{role}}
		user.Email = user.ID.String() + "@test.local"
		if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, display_name, role)
			VALUES ($1, $2, 'playground', $3)`, user.ID, user.Email, role); err != nil {
			t.Fatal(err)
		}
		return user
	}
	f.student, f.other = makeUser(auth.RoleStudent), makeUser(auth.RoleStudent)
	f.author, f.admin = makeUser(auth.RoleAuthor), makeUser(auth.RoleAdmin)
	return f
}

func TestProjectLimitConcurrent(t *testing.T) {
	f := setup(t)
	ctx := auth.WithPrincipal(t.Context(), f.student)
	var group sync.WaitGroup
	results := make(chan error, 30)
	for range 30 {
		group.Go(func() { _, err := f.svc.CreateProject(ctx, validCreate()); results <- err })
	}
	group.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else {
			wantCode(t, err, codes.ResourceExhausted)
		}
	}
	if accepted != 20 {
		t.Fatalf("accepted=%d", accepted)
	}
	projects, err := f.repo.List(ctx, f.student.ID, uuid.Nil, 100)
	if err != nil || len(projects) != 20 {
		t.Fatalf("projects=%d err=%v", len(projects), err)
	}
	if err := f.repo.Delete(ctx, projects[0].ID, f.student.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateProject(ctx, validCreate()); err != nil {
		t.Fatal(err)
	}
}

func TestCreateIdempotenceConcurrent(t *testing.T) {
	f := setup(t)
	ctx := auth.WithPrincipal(t.Context(), f.student)
	request := validCreate()
	var group sync.WaitGroup
	ids := make(chan string, 8)
	for range 8 {
		group.Go(func() {
			response, err := f.svc.CreateProject(ctx, request)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- response.Project.Id
		})
	}
	group.Wait()
	close(ids)
	var first string
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatal("duplicate projects")
		}
	}
	projects, err := f.repo.List(ctx, f.student.ID, uuid.Nil, 100)
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects=%d err=%v", len(projects), err)
	}
}
