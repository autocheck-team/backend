package judge

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Queue — операции аренды и завершения, независимые от выполнения программы.
type Queue interface {
	Claim(ctx context.Context, owner string) (Job, error)
	Heartbeat(ctx context.Context, j Job) error
	Finish(ctx context.Context, j Job, o Outcome) error
	Fail(ctx context.Context, j Job, cause error, permanent bool) error
	Release(ctx context.Context, j Job) error
}

// Executor выполняет одну попытку проверки.
type Executor interface {
	Run(ctx context.Context, j Job) (Outcome, error)
}

// Worker обслуживает пул горутин, не оставляя работающих проверок после остановки.
type Worker struct {
	Queue             Queue
	Runner            Executor
	Log               *slog.Logger
	ID                string
	Concurrency       int
	PollInterval      time.Duration
	heartbeatInterval time.Duration
}

// Run завершается после отмены проверок и снятия принадлежащих им аренд.
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for range w.Concurrency {
		wg.Go(func() {
			// Уникальная сессия защищает от совпадающих WORKER_ID в нескольких процессах.
			owner := w.ID + "/" + uuid.NewString()
			for ctx.Err() == nil {
				j, err := w.Queue.Claim(ctx, owner)
				if err == nil {
					w.process(ctx, j)
					continue
				}
				if !errors.Is(err, pgx.ErrNoRows) && ctx.Err() == nil {
					w.Log.Error("claim job", "err", err)
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
	wg.Wait()
}

func (w *Worker) process(parent context.Context, j Job) {
	ctx, cancel := context.WithCancel(parent)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		interval := w.heartbeatInterval
		if interval <= 0 {
			interval = 20 * time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := w.Queue.Heartbeat(ctx, j); err != nil {
					w.Log.Warn("job lease renewal failed", "job", j.ID, "err", err)
					cancel()
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		<-heartbeatDone
		if parent.Err() != nil {
			cleanup, stop := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
			defer stop()
			if err := w.Queue.Release(cleanup, j); err != nil && !errors.Is(err, ErrLeaseLost) {
				w.Log.Error("release job", "job", j.ID, "err", err)
			}
		}
	}()
	var outcome Outcome
	var err error
	if j.Attempts > 3 {
		err = errors.New("job attempt limit exceeded")
	} else {
		outcome, err = w.Runner.Run(ctx, j)
	}
	if ctx.Err() == nil {
		if err == nil {
			err = w.Queue.Finish(ctx, j, outcome)
		}
		// Ошибка фиксации тоже системная: если БД снова доступна, планируем обычный
		// backoff, иначе аренда истечёт и задание подхватит другой воркер.
		if err != nil && !errors.Is(err, ErrLeaseLost) && ctx.Err() == nil {
			err = w.Queue.Fail(ctx, j, err, errors.Is(err, ErrNoTests))
		}
		if err != nil {
			w.Log.Error("complete job attempt", "job", j.ID, "err", err)
		}
	}
}
