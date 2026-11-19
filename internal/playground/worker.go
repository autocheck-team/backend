package playground

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Worker обслуживает очередь playground независимо от очереди проверок посылок.
type Worker struct {
	Repository   *Repository
	Runner       *Runner
	Log          *slog.Logger
	ID           string
	Concurrency  int
	PollInterval time.Duration
}

// Run ожидает остановки всех запусков и освобождения аренд при SIGTERM.
func (w *Worker) Run(ctx context.Context) {
	var group sync.WaitGroup
	for range w.Concurrency {
		group.Go(func() {
			owner := w.ID + "/playground/" + uuid.NewString()
			for ctx.Err() == nil {
				job, err := w.Repository.Claim(ctx, owner)
				if err == nil {
					w.process(ctx, job)
					continue
				}
				if !errors.Is(err, pgx.ErrNoRows) && ctx.Err() == nil {
					w.Log.ErrorContext(ctx, "claim playground job", "err", err)
				}
				timer := time.NewTimer(w.PollInterval)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		})
	}
	group.Wait()
}

func (w *Worker) process(parent context.Context, job Job) {
	ctx, cancel := context.WithCancel(parent)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := w.Repository.Heartbeat(ctx, job); err != nil {
					w.Log.WarnContext(ctx, "renew playground lease", "run_id", job.RunID, "err", err)
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-heartbeatDone }()
	run, err := w.Repository.GetRun(ctx, job.RunID)
	if err == nil && job.Attempts <= 3 {
		var result Result
		result, err = w.Runner.Execute(ctx, run, func(ctx context.Context, stage Status) error { return w.Repository.Stage(ctx, job, stage) })
		if err == nil {
			err = w.Repository.Finish(ctx, job, result)
		}
	} else if err == nil {
		err = errors.New("playground attempt limit exceeded")
	}
	if err == nil {
		w.Log.InfoContext(ctx, "project run completed", "run_id", job.RunID)
		return
	}
	if parent.Err() == nil {
		w.Log.ErrorContext(ctx, "project run failed", "run_id", job.RunID, "err", err)
	}
	cleanup, stop := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer stop()
	if err := w.Repository.Retry(cleanup, job, parent.Err() != nil); err != nil && !errors.Is(err, ErrLeaseLost) {
		w.Log.ErrorContext(cleanup, "return playground job", "run_id", job.RunID, "err", err)
	}
}
