// Команда migrate применяет миграции БД: `migrate up` или `migrate down`.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/autocheck-team/backend/internal/config"
	"github.com/autocheck-team/backend/internal/db"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(); err != nil {
		log.Error("migrate failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cmd := "up"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	switch cmd {
	case "up":
		return db.Migrate(ctx, pool)
	case "down":
		return db.Rollback(ctx, pool)
	default:
		return fmt.Errorf("unknown command %q, want up or down", cmd)
	}
}
