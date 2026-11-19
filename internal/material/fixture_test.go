package material

import (
	"context"
	"os"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/autocheck-team/backend/internal/auth"
	"github.com/autocheck-team/backend/internal/billing"
	"github.com/autocheck-team/backend/internal/db"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

type fixture struct {
	pool      *pgxpool.Pool
	repo      *Repository
	svc       *Service
	objects   *objectStub
	problemID uuid.UUID
	secondID  uuid.UUID
	student   auth.Principal
	other     auth.Principal
	author    auth.Principal
	admin     auth.Principal
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
	schema := "material_" + uuid.New().String()[:8]
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
	f := &fixture{pool: pool, repo: NewRepository(pool, 5*time.Second), objects: &objectStub{data: make(map[string][]byte)}}
	f.svc = New(f.repo, billing.NewRepository(pool), f.objects, testValidator(t), Config{MaxAttachmentBytes: 1024, ObjectTimeout: 5 * time.Second})
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

	for _, destination := range []*uuid.UUID{&f.problemID, &f.secondID} {
		if err := pool.QueryRow(ctx, `INSERT INTO problems (author_id,slug,title,statement,time_limit_ms,memory_limit_kb,is_published)
   VALUES ($1,$2,'Test','Test',1000,262144,true) RETURNING id`, f.author.ID, uuid.NewString()).Scan(destination); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func testValidator(t *testing.T) protovalidate.Validator {
	t.Helper()
	validator, err := protovalidate.New()
	if err != nil {
		t.Fatal(err)
	}
	return validator
}

func (f *fixture) input() *v1.MaterialInput {
	return &v1.MaterialInput{Title: "Разбор", Description: "Описание", ContentMarkdown: "# Решение", ProblemIds: []string{f.problemID.String()}, Ordinal: 1, IsPublished: true}
}

func (f *fixture) create(t *testing.T, published bool, ordinal int32) Material {
	t.Helper()
	result, err := f.repo.Create(t.Context(), Material{
		RequestID: uuid.New(), Title: "Разбор", Description: "Описание", ContentMarkdown: "# Решение",
		ProblemIDs: []uuid.UUID{f.problemID, f.secondID}, Ordinal: ordinal, IsPublished: published,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (f *fixture) subscribe(t *testing.T, principal auth.Principal, state string, start, end time.Time) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), `INSERT INTO subscriptions (user_id,plan_code,status,starts_at,ends_at) VALUES ($1,'pro',$2,$3,$4)
 ON CONFLICT(user_id) DO UPDATE SET status=excluded.status,starts_at=excluded.starts_at,ends_at=excluded.ends_at`, principal.ID, state, start, end)
	if err != nil {
		t.Fatal(err)
	}
}
