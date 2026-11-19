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

func TestTestGenerationMigration(t *testing.T) {
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
	schema := "migration_generation_" + uuid.NewString()[:8]
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
		if file.Name() >= "00012" {
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
	var testID uuid.UUID
	err = pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		submissionID := seed(t, tx)
		return tx.QueryRow(t.Context(), `INSERT INTO test_cases (problem_id,ordinal,input_key,expected_key,is_ready,input_size,expected_size)
   SELECT problem_id,1,'original-input','original-expected',true,12,34 FROM submissions WHERE id=$1 RETURNING id`, submissionID).Scan(&testID)
	})
	if err != nil {
		t.Fatal(err)
	}
	// act
	if err := Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	// assert
	var generated, ready bool
	var inputKey, expectedKey string
	var inputSize, expectedSize int64
	if err := pool.QueryRow(t.Context(), `SELECT is_generated,is_ready,input_key,expected_key,input_size,expected_size FROM test_cases WHERE id=$1`, testID).Scan(&generated, &ready, &inputKey, &expectedKey, &inputSize, &expectedSize); err != nil {
		t.Fatal(err)
	}
	if generated || !ready || inputKey != "original-input" || expectedKey != "original-expected" || inputSize != 12 || expectedSize != 34 {
		t.Fatal("existing test changed by migration")
	}
	for code, want := range map[string]int{"free": 0, "pro": 10} {
		var limit int
		if err := pool.QueryRow(t.Context(), `SELECT daily_test_generation_limit FROM plans WHERE code=$1`, code).Scan(&limit); err != nil || limit != want {
			t.Fatalf("invalid plan generation limit for %s", code)
		}
	}
	var tables int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.tables WHERE table_schema=$1 AND table_name IN ('test_generations','generated_tests')`, schema).Scan(&tables); err != nil || tables != 2 {
		t.Fatal("generation tables missing")
	}
	t.Log("00012 upgrade preserved existing test data and installed tables, defaults and quotas")
}
