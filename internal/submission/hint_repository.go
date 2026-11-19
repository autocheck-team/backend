package submission

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/autocheck-team/backend/internal/billing"
)

var (
	errHintsDisabled  = errors.New("problem does not allow AI hints")
	errHintInProgress = errors.New("hint generation in progress")
	errHintLeaseLost  = errors.New("hint generation lease lost")
)

// Hint содержит только проверенный ответ; исходник и промпт в истории не сохраняются.
type Hint struct {
	ID         int64
	Text       string
	IsFallback bool
	CreatedAt  time.Time
}

// HintRepository резервирует квоту до внешнего вызова и сохраняет единственный ответ посылки.
type HintRepository struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

// NewHintRepository задаёт дедлайн коротких операций БД из конфигурации.
func NewHintRepository(pool *pgxpool.Pool, timeout time.Duration) *HintRepository {
	return &HintRepository{pool: pool, timeout: timeout}
}

// Problem возвращает условие только при текущем разрешении автора.
func (r *HintRepository) Problem(ctx context.Context, problemID uuid.UUID) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	var statement string
	var allowed bool
	err := r.pool.QueryRow(ctx, `SELECT statement,allow_ai_hints FROM problems WHERE id=$1`, problemID).Scan(&statement, &allowed)
	if err != nil {
		return "", fmt.Errorf("read hint problem: %w", err)
	}
	if !allowed {
		return "", errHintsDisabled
	}
	return statement, nil
}

// Get возвращает готовый ответ; незавершённый резерв не является историей.
func (r *HintRepository) Get(ctx context.Context, submissionID uuid.UUID) (Hint, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	hint, err := scanHint(r.pool.QueryRow(ctx, `SELECT id,text,is_fallback,updated_at FROM submission_hints WHERE submission_id=$1 AND text<>''`, submissionID))
	if err != nil {
		return Hint{}, fmt.Errorf("read submission hint: %w", err)
	}
	return hint, nil
}

func scanHint(row pgx.Row) (Hint, error) {
	var hint Hint
	err := row.Scan(&hint.ID, &hint.Text, &hint.IsFallback, &hint.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return hint, ErrNotFound
	}
	return hint, err
}

// Reserve сериализует новые подсказки пользователя, включая разные посылки и экземпляры API.
// Просроченная аренда восстанавливается с новым токеном; старый вызов не сможет сохранить результат.
func (r *HintRepository) Reserve(ctx context.Context, sub Submission, token uuid.UUID, lease time.Duration) (Hint, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	var hint Hint
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var locked uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, sub.UserID).Scan(&locked); err != nil {
			return fmt.Errorf("lock hint owner: %w", err)
		}
		var allowed bool
		if err := tx.QueryRow(ctx, `SELECT allow_ai_hints FROM problems WHERE id=$1 FOR SHARE`, sub.ProblemID).Scan(&allowed); err != nil {
			return fmt.Errorf("check hint permission: %w", err)
		}
		if !allowed {
			return errHintsDisabled
		}
		var active bool
		err := tx.QueryRow(ctx, `SELECT id,text,is_fallback,updated_at,COALESCE(lease_until>now(),false) FROM submission_hints WHERE submission_id=$1`, sub.ID).
			Scan(&hint.ID, &hint.Text, &hint.IsFallback, &hint.CreatedAt, &active)
		if err == nil {
			if hint.Text != "" {
				return nil
			}
			if active {
				return errHintInProgress
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read hint reservation: %w", err)
		}
		if err := billing.CheckHint(ctx, tx, sub.UserID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO submission_hints (submission_id,user_id,lease_token,lease_until)
            VALUES ($1,$2,$3,now()+make_interval(secs=>$4))
            ON CONFLICT (submission_id) DO UPDATE SET lease_token=excluded.lease_token,
            lease_until=excluded.lease_until,reserved_at=now(),updated_at=now()`, sub.ID, sub.UserID, token, lease.Seconds())
		if err != nil {
			return fmt.Errorf("reserve submission hint: %w", err)
		}
		return nil
	})
	if err != nil {
		return Hint{}, fmt.Errorf("prepare hint generation: %w", err)
	}
	return hint, nil
}

// Complete публикует только результат владельца действующей аренды.
func (r *HintRepository) Complete(ctx context.Context, submissionID, token uuid.UUID, text string, fallback bool) (Hint, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	hint, err := scanHint(r.pool.QueryRow(ctx, `UPDATE submission_hints SET text=$3,is_fallback=$4,
        lease_token=NULL,lease_until=NULL,updated_at=now() WHERE submission_id=$1 AND lease_token=$2 AND lease_until>now()
        RETURNING id,text,is_fallback,updated_at`, submissionID, token, text, fallback))
	if errors.Is(err, ErrNotFound) {
		return Hint{}, errHintLeaseLost
	}
	if err != nil {
		return Hint{}, fmt.Errorf("complete submission hint: %w", err)
	}
	return hint, nil
}

// Release освобождает квоту после ошибки, не затрагивая новый резерв или готовую подсказку.
func (r *HintRepository) Release(ctx context.Context, submissionID, token uuid.UUID) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	_, err := r.pool.Exec(ctx, `DELETE FROM submission_hints WHERE submission_id=$1 AND lease_token=$2`, submissionID, token)
	if err != nil {
		return fmt.Errorf("release submission hint: %w", err)
	}
	return nil
}
