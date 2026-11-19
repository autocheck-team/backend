// Команда worker забирает посылки из очереди judge_jobs и проверяет их в песочнице.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/autocheck-team/backend/internal/llm"
	"github.com/autocheck-team/backend/internal/testgeneration"
	"golang.org/x/sync/errgroup"

	"github.com/autocheck-team/backend/internal/config"
	"github.com/autocheck-team/backend/internal/db"
	"github.com/autocheck-team/backend/internal/judge"
	"github.com/autocheck-team/backend/internal/sandbox"
	"github.com/autocheck-team/backend/internal/storage"
)

const sweepInterval = 5 * time.Minute

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

	sb, err := sandbox.New(cfg.SandboxRuntime)
	if err != nil {
		return err
	}
	defer func() { _ = sb.Close() }()
	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	objects, err := storage.New(initCtx, storage.Config{
		Endpoint: cfg.S3Endpoint, PublicEndpoint: cfg.S3PublicEndpoint,
		Bucket: cfg.S3Bucket, AccessKey: cfg.S3AccessKey, SecretKey: cfg.S3SecretKey,
	})
	cancel()
	if err != nil {
		return err
	}
	repo := judge.NewRepository(pool)
	worker := judge.Worker{
		Queue: repo, Runner: judge.NewRunner(repo, objects, judge.Docker{Sandbox: sb}),
		Log: log, ID: cfg.WorkerID, Concurrency: cfg.WorkerConcurrency, PollInterval: cfg.WorkerPollInterval,
	}
	log.Info("worker started", "id", cfg.WorkerID, "concurrency", cfg.WorkerConcurrency)
	model, err := llm.New(cfg.LLM)
	if err != nil {
		return err
	}
	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error { sweep(ctx, log, sb); return nil })
	group.Go(func() error { worker.Run(ctx); return nil })
	if model.Enabled() {
		group.Go(func() error {
			testgeneration.NewRepository(pool).Run(ctx, testgeneration.NewRunner(model, sb), cfg.WorkerConcurrency, cfg.WorkerPollInterval, func(err error) { log.ErrorContext(ctx, "test generation failed", "err", err) })
			return nil
		})
	}
	return group.Wait()
}

// sweep убирает остатки упавших воркеров при старте и затем периодически.
func sweep(ctx context.Context, log *slog.Logger, sb *sandbox.Sandbox) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		sweepCtx, cancel := context.WithTimeout(ctx, time.Minute)
		if err := sb.Sweep(sweepCtx); err != nil && ctx.Err() == nil {
			log.Warn("sweep sandbox leftovers", "err", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
