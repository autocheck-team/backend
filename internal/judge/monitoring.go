package judge

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/autocheck-team/backend/internal/monitoring"
)

// QueueDepth считает только незавершённые задания; просроченная аренда снова ожидает.
func (r *Repository) QueueDepth(ctx context.Context) (waiting, running int64, err error) {
	err = r.pool.QueryRow(ctx, `SELECT
 COUNT(*) FILTER (WHERE j.locked_by IS NULL OR j.locked_until IS NULL OR j.locked_until <= now()),
 COUNT(*) FILTER (WHERE j.locked_by IS NOT NULL AND j.locked_until > now())
 FROM judge_jobs j JOIN submissions s ON s.id = j.submission_id
 WHERE s.status NOT IN ('done', 'failed')`).Scan(&waiting, &running)
	if err != nil {
		return 0, 0, fmt.Errorf("read queue depth: %w", err)
	}
	return waiting, running, nil
}

// ObserveQueue опрашивает БД вне scrape-запроса, ограничивая время каждого снимка.
func ObserveQueue(ctx context.Context, repo *Repository, metrics *monitoring.Metrics, log *slog.Logger, interval, timeout time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		query, cancel := context.WithTimeout(ctx, timeout)
		waiting, running, err := repo.QueueDepth(query)
		cancel()
		metrics.Queue(waiting, running, err)
		if err != nil && ctx.Err() == nil {
			log.WarnContext(ctx, "queue metrics poll failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
