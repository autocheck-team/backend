package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrProRequired означает отсутствие действующей подписки Pro.
var ErrProRequired = errors.New("active Pro subscription required")

// GenerationLimitError сообщает время до начала следующих суток UTC.
type GenerationLimitError struct{ RetryAfter time.Duration }

// Error возвращает безопасную причину отказа.
func (e *GenerationLimitError) Error() string { return "daily test generation limit exhausted" }

// CheckTestGeneration проверяет активную Pro и дневную квоту под блокировкой строки пользователя.
// Ноль в daily_test_generation_limit запрещает генерации; queued и failed тоже расходуют квоту.
func CheckTestGeneration(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	limits, err := Snapshot(ctx, tx, userID)
	if err != nil {
		return fmt.Errorf("read generation entitlement: %w", err)
	}
	if limits.Plan.Code != "pro" {
		return ErrProRequired
	}
	var limit int
	var used int
	var retrySeconds float64
	err = tx.QueryRow(ctx, `WITH bounds AS (SELECT date_trunc('day',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS day_start)
 SELECT p.daily_test_generation_limit,
 (SELECT count(*) FROM test_generations WHERE user_id=$1 AND created_at>=b.day_start),
 extract(epoch FROM (b.day_start+interval '24 hours'-now()))::double precision
 FROM plans p CROSS JOIN bounds b WHERE p.code=$2`, userID, limits.Plan.Code).Scan(&limit, &used, &retrySeconds)
	if err != nil {
		return fmt.Errorf("read daily generation quota: %w", err)
	}
	if used >= limit {
		return &GenerationLimitError{RetryAfter: time.Duration(retrySeconds * float64(time.Second))}
	}
	return nil
}
