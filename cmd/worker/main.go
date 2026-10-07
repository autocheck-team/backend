// Команда worker забирает посылки из очереди judge_jobs и проверяет их в песочнице.
//
// Пока это заготовка: подключается к БД и ждёт сигнала остановки. Очередь
// (HPRPR-32) и песочница (HPRPR-31) будут реализованы отдельными задачами.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/autocheck-team/backend/internal/config"
	"github.com/autocheck-team/backend/internal/db"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("worker stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	log.Info("worker started", "id", cfg.WorkerID, "concurrency", cfg.WorkerConcurrency)
	<-ctx.Done()
	return nil
}
