package db

import (
	"context"
	"io/fs"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

func TestBillingMigrationPreservesRunMonth(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	admin, err := Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := "migration_" + uuid.New().String()[:8]
	if _, err := admin.Exec(t.Context(), `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`); err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// Сначала поднимаем реальную схему до биллинга: это проверяет upgrade с историческими данными.
	previous := fstest.MapFS{}
	files, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.Name() >= "00009" {
			continue
		}
		content, err := migrations.ReadFile("migrations/" + file.Name())
		if err != nil {
			t.Fatal(err)
		}
		previous[file.Name()] = &fstest.MapFile{Data: content}
	}
	sqlDB := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Error(err)
		}
	})
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, previous)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	var submissionID uuid.UUID
	var historical time.Time
	err = pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		submissionID = uuid.MustParse(seed(t, tx))
		if err := tx.QueryRow(t.Context(), `UPDATE submissions SET created_at=date_trunc('month',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'-interval '1 second' WHERE id=$1 RETURNING created_at`, submissionID).Scan(&historical); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), `WITH test AS (INSERT INTO test_cases (problem_id,ordinal,input_key,expected_key)
   SELECT problem_id,1,'in','out' FROM submissions WHERE id=$1 RETURNING id)
   INSERT INTO test_runs (submission_id,test_case_id,verdict,time_ms,memory_kb) SELECT $1,id,'OK',1,1 FROM test`, submissionID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	var actual time.Time
	if err := pool.QueryRow(t.Context(), `SELECT created_at FROM test_runs WHERE submission_id=$1`, submissionID).Scan(&actual); err != nil {
		t.Fatal(err)
	}
	if !actual.Equal(historical) {
		t.Fatalf("historical run shifted: got %s want %s", actual, historical)
	}
}
