// Package feedback хранит и обрабатывает запросы пользователей об изменениях платформы.
package feedback

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Ошибки, требующие отдельного ответа клиенту.
var (
	ErrNotFound         = errors.New("feedback request not found")
	ErrUnknownLanguage  = errors.New("unknown language")
	ErrConflict         = errors.New("feedback request status changed")
	errDuplicateRequest = errors.New("duplicate client request")
)

// Kind — предмет запроса, независимо от контракта API.
type Kind int32

// Типы запросов; ноль не сохраняется в БД.
const (
	KindUnspecified Kind = iota
	KindCompilerUpdate
	KindNewLibrary
	KindOther
)

// Status — состояние рассмотрения запроса.
type Status int32

// Статусы запросов; ноль не сохраняется в БД.
const (
	StatusUnspecified Status = iota
	StatusNew
	StatusAccepted
	StatusRejected
	StatusCompleted
)

// Request — запрос пользователя и результат его рассмотрения.
type Request struct {
	ID              uuid.UUID
	UserID          uuid.UUID
	Kind            Kind
	LanguageID      *string
	Title           string
	Text            string
	Status          Status
	AdminComment    string
	ClientRequestID *uuid.UUID
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ListFilter ограничивает выборку; нулевые значения отключают соответствующий фильтр.
type ListFilter struct {
	UserID uuid.UUID
	Kind   Kind
	Status Status
	After  uuid.UUID
	Limit  int
}

// Repository хранит запросы в PostgreSQL.
type Repository struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

// NewRepository задаёт пул и предельную длительность каждой операции БД.
func NewRepository(pool *pgxpool.Pool, timeout time.Duration) *Repository {
	return &Repository{pool: pool, timeout: timeout}
}

const requestColumns = `id, user_id, type, language_id, title, text, status,
	admin_comment, client_request_id, created_at, updated_at`

func scanRequest(row pgx.Row) (Request, error) {
	var request Request
	err := row.Scan(&request.ID, &request.UserID, &request.Kind, &request.LanguageID,
		&request.Title, &request.Text, &request.Status, &request.AdminComment,
		&request.ClientRequestID, &request.CreatedAt, &request.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Request{}, ErrNotFound
	}
	if err != nil {
		return Request{}, fmt.Errorf("scan feedback request: %w", err)
	}
	return request, nil
}

// Create сохраняет запрос; уникальный индекс разрешает гонку повторных отправок.
func (r *Repository) Create(ctx context.Context, request Request) (Request, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	created, err := scanRequest(r.pool.QueryRow(ctx, `
		INSERT INTO feedback_requests (id, user_id, type, language_id, title, text, client_request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+requestColumns,
		request.ID, request.UserID, request.Kind, request.LanguageID, request.Title, request.Text, request.ClientRequestID))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch {
			case pgErr.Code == pgerrcode.UniqueViolation &&
				pgErr.ConstraintName == "feedback_requests_ukey_user_id_client_request_id":
				return Request{}, errDuplicateRequest
			case pgErr.Code == pgerrcode.ForeignKeyViolation &&
				pgErr.ConstraintName == "feedback_requests_language_id_fkey":
				return Request{}, ErrUnknownLanguage
			}
		}
		return Request{}, fmt.Errorf("create feedback request: %w", err)
	}
	return created, nil
}

// FindByClientRequest ищет предыдущий результат отправки данного пользователя.
func (r *Repository) FindByClientRequest(ctx context.Context, userID, requestID uuid.UUID) (Request, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	request, err := scanRequest(r.pool.QueryRow(ctx, `SELECT `+requestColumns+`
		FROM feedback_requests WHERE user_id = $1 AND client_request_id = $2`, userID, requestID))
	if err != nil {
		return Request{}, fmt.Errorf("find feedback request by client request: %w", err)
	}
	return request, nil
}

// Get возвращает запрос без проверки прав; доступ проверяет сервис.
func (r *Repository) Get(ctx context.Context, id uuid.UUID) (Request, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	request, err := scanRequest(r.pool.QueryRow(ctx,
		`SELECT `+requestColumns+` FROM feedback_requests WHERE id = $1`, id))
	if err != nil {
		return Request{}, fmt.Errorf("get feedback request %s: %w", id, err)
	}
	return request, nil
}

// List возвращает страницу по убыванию UUIDv7 без OFFSET.
func (r *Repository) List(ctx context.Context, filter ListFilter) ([]Request, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	conditions := make([]string, 0, 4)
	args := make([]any, 0, 5)
	arg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	if filter.UserID != uuid.Nil {
		conditions = append(conditions, "user_id = "+arg(filter.UserID))
	}
	if filter.Status != StatusUnspecified {
		conditions = append(conditions, "status = "+arg(filter.Status))
	}
	if filter.Kind != KindUnspecified {
		conditions = append(conditions, "type = "+arg(filter.Kind))
	}
	if filter.After != uuid.Nil {
		conditions = append(conditions, "id < "+arg(filter.After))
	}
	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}
	rows, err := r.pool.Query(ctx, `SELECT `+requestColumns+` FROM feedback_requests`+
		where+` ORDER BY id DESC LIMIT `+arg(filter.Limit), args...)
	if err != nil {
		return nil, fmt.Errorf("list feedback requests: %w", err)
	}
	requests, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Request, error) { return scanRequest(row) })
	if err != nil {
		return nil, fmt.Errorf("collect feedback requests: %w", err)
	}
	return requests, nil
}

// Update атомарно меняет статус и комментарий, если статус не изменился после чтения.
func (r *Repository) Update(ctx context.Context, id uuid.UUID, previous, next Status, comment string) (Request, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	request, err := scanRequest(r.pool.QueryRow(ctx, `UPDATE feedback_requests
		SET status = $3, admin_comment = $4, updated_at = clock_timestamp()
		WHERE id = $1 AND status = $2 RETURNING `+requestColumns, id, previous, next, comment))
	if errors.Is(err, ErrNotFound) {
		return Request{}, ErrConflict
	}
	if err != nil {
		return Request{}, fmt.Errorf("update feedback request %s: %w", id, err)
	}
	return request, nil
}
