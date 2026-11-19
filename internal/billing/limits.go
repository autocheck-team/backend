package billing

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Queryer позволяет считать квоты в той же транзакции, которая принимает посылку.
type Queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Limits содержит эффективный тариф и расход пользователя за календарный месяц UTC.
type Limits struct {
	Plan         Plan
	Status       string
	StartsAt     *time.Time
	EndsAt       *time.Time
	Used         int64
	Reserved     int64
	Compilations int64
	RetryAfter   time.Duration
}

// Snapshot читает права и расход одним SQL-снимком, включая ещё не выполненные тесты очереди.
func Snapshot(ctx context.Context, query Queryer, userID uuid.UUID) (Limits, error) {
	var limits Limits
	var retrySeconds float64
	err := query.QueryRow(ctx, `WITH effective AS (
  SELECT COALESCE((SELECT plan_code FROM subscriptions WHERE user_id=$1 AND status='active' AND ends_at>now() AND starts_at<=now()),'free') AS code
 ), bounds AS (SELECT date_trunc('month',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS month_start),
 usage AS (
  SELECT s.id,s.status,s.test_run_budget,
   count(tr.test_case_id) FILTER (WHERE tr.created_at>=b.month_start) AS used,
   count(tr.test_case_id) AS completed
  FROM submissions s CROSS JOIN bounds b LEFT JOIN test_runs tr ON tr.submission_id=s.id
  WHERE s.user_id=$1 GROUP BY s.id
 ), compilations AS (
  SELECT s.created_at FROM submissions s JOIN languages l ON l.id=s.language_id
  JOIN plans p ON p.code=(SELECT code FROM effective)
  WHERE s.user_id=$1 AND cardinality(l.compile_cmd)>0
   AND s.created_at>now()-make_interval(secs=>p.compilation_window_seconds)
  UNION ALL
  SELECT r.created_at FROM playground_runs r
  JOIN plans p ON p.code=(SELECT code FROM effective)
  WHERE r.user_id=$1 AND r.is_compiled
   AND r.created_at>now()-make_interval(secs=>p.compilation_window_seconds)
 )
 SELECT p.code,p.name,p.amount_kopecks,p.period_days,p.compilation_limit,p.compilation_window_seconds,p.monthly_test_run_limit,p.has_material_access,
  CASE WHEN sub.user_id IS NULL THEN 'free' WHEN sub.status='active' AND sub.ends_at<=now() THEN 'expired' ELSE sub.status END,
  sub.starts_at,sub.ends_at,
  COALESCE((SELECT sum(used) FROM usage),0)::bigint,
  COALESCE((SELECT sum(GREATEST(test_run_budget-completed,0)) FROM usage WHERE status IN ('queued','compiling','running')),0)::bigint,
  (SELECT count(*) FROM compilations),
  COALESCE((SELECT GREATEST(extract(epoch FROM (created_at+make_interval(secs=>p.compilation_window_seconds)-now())),0)
   FROM compilations ORDER BY created_at DESC OFFSET GREATEST(p.compilation_limit-1,0) LIMIT 1),0)::double precision
 FROM plans p LEFT JOIN subscriptions sub ON sub.user_id=$1 WHERE p.code=(SELECT code FROM effective)`, userID).
		Scan(&limits.Plan.Code, &limits.Plan.Name, &limits.Plan.AmountKopecks, &limits.Plan.PeriodDays,
			&limits.Plan.CompilationLimit, &limits.Plan.CompilationWindowSeconds, &limits.Plan.MonthlyTestRunLimit, &limits.Plan.HasMaterialAccess,
			&limits.Status, &limits.StartsAt, &limits.EndsAt, &limits.Used, &limits.Reserved, &limits.Compilations, &retrySeconds)
	if err != nil {
		return Limits{}, fmt.Errorf("read billing limits: %w", err)
	}
	if limits.Plan.CompilationLimit > 0 && limits.Compilations >= int64(limits.Plan.CompilationLimit) {
		limits.RetryAfter = time.Duration(math.Ceil(retrySeconds)) * time.Second
	}
	return limits, nil
}

// RemainingTests вычитает резерв очереди; -1 обозначает безлимитный тариф.
func (l Limits) RemainingTests() int64 {
	if l.Plan.MonthlyTestRunLimit == 0 {
		return -1
	}
	return max(0, int64(l.Plan.MonthlyTestRunLimit)-l.Used-l.Reserved)
}

// LimitError сообщает сценарию вид отказа без зависимости хранилища от транспорта.
type LimitError struct {
	// RetryAfter положителен для окна компиляций, ноль означает месячную квоту прогонов.
	RetryAfter time.Duration
}

// Error возвращает безопасное описание причины отказа.
func (e *LimitError) Error() string {
	if e.RetryAfter > 0 {
		return "compilation limit exhausted"
	}
	return "monthly test run limit exhausted"
}

// CheckSubmission вызывается под блокировкой пользователя, до вставки посылки и задания.
func CheckSubmission(ctx context.Context, tx pgx.Tx, userID, problemID uuid.UUID, languageID string) (int32, error) {
	limits, err := Snapshot(ctx, tx, userID)
	if err != nil {
		return 0, err
	}
	var compiled bool
	var tests int32
	err = tx.QueryRow(ctx, `SELECT COALESCE(cardinality(compile_cmd)>0,false) FROM languages WHERE id=$1`, languageID).Scan(&compiled)
	if err != nil {
		return 0, fmt.Errorf("read language compilation step: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM test_cases WHERE problem_id=$1 AND is_ready`, problemID).Scan(&tests); err != nil {
		return 0, fmt.Errorf("count ready tests: %w", err)
	}
	if limits.Plan.MonthlyTestRunLimit > 0 && (limits.RemainingTests() == 0 || int64(tests) > limits.RemainingTests()) {
		return 0, &LimitError{}
	}
	if compiled && limits.RetryAfter > 0 {
		return 0, &LimitError{RetryAfter: limits.RetryAfter}
	}
	return tests, nil
}

// Limits возвращает снимок квот для авторизованного пользователя.
func (r *Repository) Limits(ctx context.Context, userID uuid.UUID) (Limits, error) {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	return Snapshot(ctx, r.pool, userID)
}

// CheckCompilation проверяет общую квоту компиляций под блокировкой строки users.
// Интерпретируемые запуски не расходуют квоту компиляций или тестовых прогонов.
func CheckCompilation(ctx context.Context, tx pgx.Tx, userID uuid.UUID, compiled bool) error {
	if !compiled {
		return nil
	}
	limits, err := Snapshot(ctx, tx, userID)
	if err != nil {
		return fmt.Errorf("check compilation quota: %w", err)
	}
	if limits.RetryAfter > 0 {
		return &LimitError{RetryAfter: limits.RetryAfter}
	}
	return nil
}
