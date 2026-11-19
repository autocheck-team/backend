// Команда worker забирает посылки из очереди judge_jobs и проверяет их в песочнице.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/autocheck-team/backend/internal/config"
	"github.com/autocheck-team/backend/internal/db"
	"github.com/autocheck-team/backend/internal/judge"
	"github.com/autocheck-team/backend/internal/playground"
	"github.com/autocheck-team/backend/internal/sandbox"
	"github.com/autocheck-team/backend/internal/storage"

	"github.com/autocheck-team/backend/internal/monitoring"
)

const sweepInterval = 5 * time.Minute

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "worker")
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
	defer func() {
		if err := sb.Close(); err != nil {
			log.Warn("close sandbox client", "err", err)
		}
	}()
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
	metrics := monitoring.New("worker")
	runner := judge.NewRunner(repo, objects, judge.Docker{Sandbox: sb})
	runner.Metrics = metrics
	worker := judge.Worker{
		Metrics: metrics,
		Queue:   repo, Runner: runner,
		Log: log, ID: cfg.WorkerID, Concurrency: cfg.WorkerConcurrency, PollInterval: cfg.WorkerPollInterval,
	}
	log.Info("worker started", "id", cfg.WorkerID, "concurrency", cfg.WorkerConcurrency)
	playgroundWorker := playground.Worker{
		Repository: playground.NewRepository(pool, cfg.PlaygroundQueryTimeout),
		Runner:     playground.NewRunner(sb), Log: log, ID: cfg.WorkerID,
		Concurrency: cfg.WorkerConcurrency, PollInterval: cfg.WorkerPollInterval,
	}
	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error { worker.Run(ctx); return nil })
	group.Go(func() error { playgroundWorker.Run(ctx); return nil })
	group.Go(func() error { sweep(ctx, log, sb, metrics); return nil })
	group.Go(func() error {
		return judge.ObserveQueue(ctx, repo, metrics, log, cfg.MetricsPollInterval, cfg.MetricsQueryTimeout)
	})
	group.Go(func() error { return metrics.Run(ctx, cfg.MetricsAddr, cfg.ShutdownTimeout) })
	return group.Wait()
}

// sweep убирает остатки упавших воркеров при старте и затем периодически.
func sweep(ctx context.Context, log *slog.Logger, sb *sandbox.Sandbox, metrics *monitoring.Metrics) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		sweepCtx, cancel := context.WithTimeout(ctx, time.Minute)
		err := sb.Sweep(sweepCtx)
		metrics.Cleanup("sweep", err)
		if err != nil && ctx.Err() == nil {
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
