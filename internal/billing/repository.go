package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const databaseTimeout = 5 * time.Second

// ErrNotFound означает отсутствие тарифа или платежа.
var ErrNotFound = errors.New("billing record not found")

// Plan хранит изменяемые данные тарифа, нулевые лимиты означают отсутствие ограничений.
type Plan struct {
	Code                     string
	Name                     string
	AmountKopecks            int64
	PeriodDays               int32
	CompilationLimit         int32
	CompilationWindowSeconds int32
	MonthlyTestRunLimit      int32
	HasMaterialAccess        bool
}

// Payment хранит снимок условий покупки и устойчивый ключ повторов API провайдера.
type Payment struct {
	ID              uuid.UUID
	UserID          uuid.UUID
	PlanCode        string
	RequestID       uuid.UUID
	ProviderID      *string
	AmountKopecks   int64
	PeriodDays      int32
	Status          string
	IdempotenceKey  uuid.UUID
	ReturnURL       string
	ConfirmationURL string
	CreatedAt       time.Time
}

// Repository хранит биллинг в PostgreSQL.
type Repository struct{ pool *pgxpool.Pool }

// NewRepository создаёт хранилище биллинга.
func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

const planColumns = `code,name,amount_kopecks,period_days,compilation_limit,compilation_window_seconds,monthly_test_run_limit,has_material_access`

func scanPlan(row pgx.Row) (Plan, error) {
	var plan Plan
	err := row.Scan(&plan.Code, &plan.Name, &plan.AmountKopecks, &plan.PeriodDays, &plan.CompilationLimit, &plan.CompilationWindowSeconds, &plan.MonthlyTestRunLimit, &plan.HasMaterialAccess)
	if errors.Is(err, pgx.ErrNoRows) {
		return Plan{}, ErrNotFound
	}
	return plan, err
}

// Plans возвращает каталог в стабильном порядке.
func (r *Repository) Plans(ctx context.Context) ([]Plan, error) {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	rows, err := r.pool.Query(ctx, `SELECT `+planColumns+` FROM plans ORDER BY amount_kopecks,code`)
	if err != nil {
		return nil, fmt.Errorf("list plans: %w", err)
	}
	plans, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Plan, error) { return scanPlan(row) })
	if err != nil {
		return nil, fmt.Errorf("read plans: %w", err)
	}
	return plans, nil
}

const paymentColumns = `id,user_id,plan_code,request_id,provider_id,amount_kopecks,period_days,status,idempotence_key,return_url,confirmation_url,created_at`

func scanPayment(row pgx.Row) (Payment, error) {
	var payment Payment
	err := row.Scan(&payment.ID, &payment.UserID, &payment.PlanCode, &payment.RequestID, &payment.ProviderID, &payment.AmountKopecks, &payment.PeriodDays, &payment.Status, &payment.IdempotenceKey, &payment.ReturnURL, &payment.ConfirmationURL, &payment.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, ErrNotFound
	}
	return payment, err
}

// Prepare записывает намерение до внешнего вызова: потеря ответа провайдера восстанавливается фоном.
func (r *Repository) Prepare(ctx context.Context, userID, requestID uuid.UUID, planCode, returnURL string) (Payment, error) {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	var payment Payment
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// Уникальность в БД защищает также от параллельных запросов на разных экземплярах API.
		_, err := tx.Exec(ctx, `INSERT INTO payments (user_id,request_id,plan_code,amount_kopecks,period_days,idempotence_key,return_url)
   SELECT $1,$2,code,amount_kopecks,period_days,$3,$4 FROM plans WHERE code=$5 AND amount_kopecks>0
   ON CONFLICT (user_id,request_id) DO NOTHING`, userID, requestID, uuid.New(), returnURL, planCode)
		if err != nil {
			return fmt.Errorf("prepare payment: %w", err)
		}
		payment, err = scanPayment(tx.QueryRow(ctx, `SELECT `+paymentColumns+` FROM payments WHERE user_id=$1 AND request_id=$2`, userID, requestID))
		return err
	})
	if err != nil {
		return Payment{}, fmt.Errorf("prepare checkout: %w", err)
	}
	return payment, nil
}

// ByProvider ищет только локально известные платежи, чтобы webhook не активировал чужую покупку.
func (r *Repository) ByProvider(ctx context.Context, id string) (Payment, error) {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	payment, err := scanPayment(r.pool.QueryRow(ctx, `SELECT `+paymentColumns+` FROM payments WHERE provider_id=$1`, id))
	if err != nil {
		return Payment{}, fmt.Errorf("find provider payment: %w", err)
	}
	return payment, nil
}

// Attach сохраняет идентификатор провайдера до последующего GET и не откатывает конечный статус.
func (r *Repository) Attach(ctx context.Context, payment Payment, provider ProviderPayment) error {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	tag, err := r.pool.Exec(ctx, `UPDATE payments SET provider_id=$2,confirmation_url=$3,updated_at=now()
  WHERE id=$1 AND (provider_id IS NULL OR provider_id=$2)`, payment.ID, provider.ID, provider.Confirmation.URL)
	if err != nil {
		return fmt.Errorf("attach provider payment: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("provider payment id mismatch")
	}
	return nil
}

// Apply активирует подписку в одной транзакции с переходом pending → succeeded.
func (r *Repository) Apply(ctx context.Context, payment Payment, provider ProviderPayment) error {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		current, err := scanPayment(tx.QueryRow(ctx, `SELECT `+paymentColumns+` FROM payments WHERE id=$1 FOR UPDATE`, payment.ID))
		if err != nil {
			return fmt.Errorf("lock payment: %w", err)
		}
		if err := validatePayment(current, provider); err != nil {
			return err
		}
		if current.Status != "pending" || provider.Status == "pending" || provider.Status == "waiting_for_capture" {
			return nil
		}
		// Разные оплаты одного пользователя сериализуются тем же замком, что и приём посылок.
		if _, err := tx.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, current.UserID); err != nil {
			return fmt.Errorf("lock subscriber: %w", err)
		}
		if provider.Status == "succeeded" {
			_, err = tx.Exec(ctx, `INSERT INTO subscriptions (user_id,plan_code,status,starts_at,ends_at)
    VALUES ($1,$2,'active',now(),now()+make_interval(days=>$3))
    ON CONFLICT (user_id) DO UPDATE SET plan_code=excluded.plan_code,status='active',
    starts_at=CASE WHEN subscriptions.status='active' AND subscriptions.ends_at>now() THEN subscriptions.starts_at ELSE now() END,
    ends_at=GREATEST(CASE WHEN subscriptions.status='active' THEN subscriptions.ends_at ELSE now() END,now())+make_interval(days=>$3),updated_at=now()`, current.UserID, current.PlanCode, current.PeriodDays)
			if err != nil {
				return fmt.Errorf("activate subscription: %w", err)
			}
		}
		_, err = tx.Exec(ctx, `UPDATE payments SET status=$2,updated_at=now() WHERE id=$1`, current.ID, provider.Status)
		if err != nil {
			return fmt.Errorf("settle payment: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("apply payment: %w", err)
	}
	return nil
}

func validatePayment(payment Payment, provider ProviderPayment) error {
	if payment.ProviderID != nil && *payment.ProviderID != provider.ID {
		return errors.New("provider payment id mismatch")
	}
	if provider.Amount.Currency != "RUB" || provider.Amount.Value != amountString(payment.AmountKopecks) || provider.Metadata["payment_id"] != payment.ID.String() {
		return errors.New("provider payment terms mismatch")
	}
	switch provider.Status {
	case "pending", "waiting_for_capture", "canceled":
		return nil
	case "succeeded":
		if provider.Paid {
			return nil
		}
	}
	return errors.New("invalid provider payment state")
}

// Pending ограничивает одну порцию и откладывает повтор, чтобы неисправный платёж не блокировал очередь.
func (r *Repository) Pending(ctx context.Context) ([]Payment, error) {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	rows, err := r.pool.Query(ctx, `WITH due AS (SELECT id FROM payments WHERE status='pending' AND next_sync_at<=now()
  ORDER BY next_sync_at,id LIMIT 50 FOR UPDATE SKIP LOCKED)
  UPDATE payments p SET next_sync_at=now()+interval '30 seconds' FROM due WHERE p.id=due.id
  RETURNING p.id,p.user_id,p.plan_code,p.request_id,p.provider_id,p.amount_kopecks,p.period_days,p.status,p.idempotence_key,p.return_url,p.confirmation_url,p.created_at`)
	if err != nil {
		return nil, fmt.Errorf("claim pending payments: %w", err)
	}
	payments, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Payment, error) { return scanPayment(row) })
	if err != nil {
		return nil, fmt.Errorf("read pending payments: %w", err)
	}
	return payments, nil
}

// Payments возвращает платежи владельца с keyset-пагинацией.
func (r *Repository) Payments(ctx context.Context, userID, after uuid.UUID, limit int) ([]Payment, error) {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	rows, err := r.pool.Query(ctx, `SELECT `+paymentColumns+` FROM payments WHERE user_id=$1 AND ($2::uuid='00000000-0000-0000-0000-000000000000' OR id<$2) ORDER BY id DESC LIMIT $3`, userID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list payments: %w", err)
	}
	payments, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Payment, error) { return scanPayment(row) })
	if err != nil {
		return nil, fmt.Errorf("read payments: %w", err)
	}
	return payments, nil
}
