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

func TestRecommendationMigration(t *testing.T) {
	// arrange
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	admin, err := Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := "migration_recommendation_" + uuid.NewString()[:8]
	if _, err := admin.Exec(t.Context(), `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`); err != nil {
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
	previous := fstest.MapFS{}
	files, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.Name() >= "00013" {
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
	var problemID string
	var title, statement string
	err = pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		submissionID := seed(t, tx)
		return tx.QueryRow(t.Context(), `SELECT p.id,p.title,p.statement FROM problems p JOIN submissions s ON s.problem_id=p.id WHERE s.id=$1`, submissionID).Scan(&problemID, &title, &statement)
	})
	if err != nil {
		t.Fatal(err)
	}
	// act
	if err := Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	// assert
	var afterTitle, afterStatement string
	if err := pool.QueryRow(t.Context(), `SELECT title,statement FROM problems WHERE id=$1`, problemID).Scan(&afterTitle, &afterStatement); err != nil || title != afterTitle || statement != afterStatement {
		t.Fatal("existing problem changed")
	}
	var tables int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.tables WHERE table_schema=$1 AND table_name IN ('problem_embeddings','recommendation_usage')`, schema).Scan(&tables); err != nil || tables != 2 {
		t.Fatal("recommendation tables missing")
	}
	var dataType, elementType string
	if err := pool.QueryRow(t.Context(), `SELECT data_type,udt_name FROM information_schema.columns WHERE table_schema=$1 AND table_name='problem_embeddings' AND column_name='embedding'`, schema).Scan(&dataType, &elementType); err != nil || dataType != "ARRAY" || elementType != "_float4" {
		t.Fatal("embedding is not float4[]")
	}
	var constraints int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_constraint WHERE connamespace=current_schema()::regnamespace
        AND contype IN ('p','f','c') AND conrelid IN ('problem_embeddings'::regclass,'recommendation_usage'::regclass)`).Scan(&constraints); err != nil || constraints != 7 {
		t.Fatalf("constraints=%d err=%v", constraints, err)
	}
	t.Log("00013 upgrade preserved problem data; float4[] storage and quota constraints installed")
}
