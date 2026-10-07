// Package db отвечает за подключение к PostgreSQL и миграции схемы.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Connect открывает пул соединений и проверяет, что база доступна.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// Migrate применяет все миграции из migrations/ до последней версии.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()
	return runGoose(sqlDB, func(p *goose.Provider) error {
		_, err := p.Up(ctx)
		return err
	})
}

// Rollback откатывает последнюю применённую миграцию.
func Rollback(ctx context.Context, pool *pgxpool.Pool) error {
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()
	return runGoose(sqlDB, func(p *goose.Provider) error {
		_, err := p.Down(ctx)
		return err
	})
}

func runGoose(sqlDB *sql.DB, fn func(*goose.Provider) error) error {
	dir, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, dir)
	if err != nil {
		return fmt.Errorf("goose provider: %w", err)
	}
	if err := fn(p); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}
