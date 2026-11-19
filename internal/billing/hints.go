package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// HintLimitError сообщает время до обновления квоты в полночь UTC.
type HintLimitError struct{ RetryAfter time.Duration }

// Error возвращает безопасное описание исчерпанного лимита.
func (e *HintLimitError) Error() string { return "daily AI hint limit exhausted" }

// CheckHint проверяет активную Pro и квоту под блокировкой пользователя.
// Незавершённые резервы учитываются до истечения аренды; готовые подсказки — за сутки резерва.
func CheckHint(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	limits, err := Snapshot(ctx, tx, userID)
	if err != nil {
		return fmt.Errorf("read hint entitlement: %w", err)
	}
	if limits.Plan.Code != "pro" {
		return ErrProRequired
	}
	var used int
	var retrySeconds float64
	err = tx.QueryRow(ctx, `WITH bounds AS (
        SELECT date_trunc('day',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS day_start)
        SELECT count(h.id), extract(epoch FROM (b.day_start+interval '24 hours'-now()))::double precision
        FROM bounds b LEFT JOIN submission_hints h ON h.user_id=$1 AND h.reserved_at>=b.day_start
        AND (h.text<>'' OR h.lease_until>now()) GROUP BY b.day_start`, userID).Scan(&used, &retrySeconds)
	if err != nil {
		return fmt.Errorf("read daily hint quota: %w", err)
	}
	if used >= int(limits.Plan.DailyAIHintLimit) {
		return &HintLimitError{RetryAfter: time.Duration(retrySeconds * float64(time.Second))}
	}
	return nil
}
