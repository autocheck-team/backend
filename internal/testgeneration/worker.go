package testgeneration

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Run обслуживает отдельный пул, чтобы задержки LLM не занимали очередь посылок.
// report получает системные ошибки без исходников и ответов провайдера.
func (r *Repository) Run(ctx context.Context, runner *Runner, concurrency int, poll time.Duration, report func(error)) {
	var group sync.WaitGroup
	for range concurrency {
		group.Go(func() {
			for ctx.Err() == nil {
				job, err := r.Claim(ctx)
				if err == nil {
					r.process(ctx, runner, job, report)
					continue
				}
				if !errors.Is(err, pgx.ErrNoRows) && ctx.Err() == nil {
					report(err)
				}
				timer := time.NewTimer(poll)
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

func (r *Repository) process(parent context.Context, runner *Runner, job Job, report func(error)) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := r.Heartbeat(ctx, job); err != nil {
					if ctx.Err() == nil {
						report(err)
					}
					cancel()
					return
				}
			}
		}
	}()
	drafts, failure := []Draft(nil), "generation interrupted"
	if job.Attempts <= 3 {
		var err error
		drafts, err = runner.Run(ctx, job.Task)
		failure = ""
		if err != nil {
			failure = "generation failed: provider or sandbox unavailable, or invalid model response"
			if ctx.Err() == nil {
				report(err)
			}
		}
	}
	if ctx.Err() == nil || (parent.Err() == nil && errors.Is(ctx.Err(), context.DeadlineExceeded)) {
		saveCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
		if err := r.Finish(saveCtx, job, drafts, failure); err != nil {
			report(err)
		}
		stop()
	}
	cancel()
	<-done
	if parent.Err() != nil {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(parent), queryTimeout)
		defer stop()
		if err := r.Release(cleanup, job); err != nil {
			report(err)
		}
	}
}
