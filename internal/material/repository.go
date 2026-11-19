package material

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository хранит материалы в PostgreSQL.
type Repository struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

// NewRepository задаёт пул и таймаут операций БД.
func NewRepository(pool *pgxpool.Pool, timeout time.Duration) *Repository {
	return &Repository{pool: pool, timeout: timeout}
}

const materialColumns = `m.id,m.request_id,m.title,m.description,m.content_markdown,m.ordinal,
 m.is_published,m.created_at,m.updated_at,
 ARRAY(SELECT problem_id FROM material_problems WHERE material_id=m.id ORDER BY problem_id)`

func scanMaterial(row pgx.Row) (Material, error) {
	var material Material
	err := row.Scan(&material.ID, &material.RequestID, &material.Title, &material.Description,
		&material.ContentMarkdown, &material.Ordinal, &material.IsPublished, &material.CreatedAt,
		&material.UpdatedAt, &material.ProblemIDs)
	if err != nil {
		return Material{}, repositoryError(err)
	}
	return material, nil
}

func repositoryError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) && postgresError.Code == pgerrcode.ForeignKeyViolation {
		if postgresError.ConstraintName == "material_problems_fkey_problem_id" {
			return ErrUnknownProblem
		}
		return ErrNotFound
	}
	return fmt.Errorf("access material storage: %w", err)
}

// Create сохраняет материал и связи атомарно; повтор request_id возвращает первый результат.
func (r *Repository) Create(ctx context.Context, material Material) (Material, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	var result Material
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var id uuid.UUID
		err := tx.QueryRow(ctx, `INSERT INTO materials (request_id,title,description,content_markdown,ordinal,is_published)
   VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (request_id) DO NOTHING RETURNING id`,
			material.RequestID, material.Title, material.Description, material.ContentMarkdown, material.Ordinal, material.IsPublished).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			result, err = scanMaterial(tx.QueryRow(ctx, `SELECT `+materialColumns+` FROM materials m WHERE request_id=$1`, material.RequestID))
			return err
		}
		if err != nil {
			return repositoryError(err)
		}
		if err := replaceProblems(ctx, tx, id, material.ProblemIDs); err != nil {
			return err
		}
		result, err = scanMaterial(tx.QueryRow(ctx, `SELECT `+materialColumns+` FROM materials m WHERE m.id=$1`, id))
		return err
	})
	if err != nil {
		return Material{}, fmt.Errorf("create material: %w", err)
	}
	return result, nil
}

func replaceProblems(ctx context.Context, tx pgx.Tx, id uuid.UUID, problems []uuid.UUID) error {
	if _, err := tx.Exec(ctx, `DELETE FROM material_problems WHERE material_id=$1`, id); err != nil {
		return fmt.Errorf("replace material problems: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO material_problems (material_id,problem_id) SELECT $1,unnest($2::uuid[])`, id, problems); err != nil {
		return repositoryError(err)
	}
	return nil
}

// Update заменяет текст, публикацию и связи в одной транзакции.
func (r *Repository) Update(ctx context.Context, material Material) (Material, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	var result Material
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE materials SET title=$2,description=$3,content_markdown=$4,ordinal=$5,
   is_published=$6,updated_at=clock_timestamp() WHERE id=$1`, material.ID, material.Title, material.Description,
			material.ContentMarkdown, material.Ordinal, material.IsPublished)
		if err != nil {
			return repositoryError(err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if err := replaceProblems(ctx, tx, material.ID, material.ProblemIDs); err != nil {
			return err
		}
		result, err = scanMaterial(tx.QueryRow(ctx, `SELECT `+materialColumns+` FROM materials m WHERE m.id=$1`, material.ID))
		return err
	})
	if err != nil {
		return Material{}, fmt.Errorf("update material: %w", err)
	}
	return result, nil
}

// Get возвращает материал вместе со связями одним SQL-снимком.
func (r *Repository) Get(ctx context.Context, id uuid.UUID) (Material, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return scanMaterial(r.pool.QueryRow(ctx, `SELECT `+materialColumns+` FROM materials m WHERE m.id=$1`, id))
}

// List выбирает страницу без OFFSET; черновики доступны только при All.
func (r *Repository) List(ctx context.Context, filter ListFilter) ([]Material, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	rows, err := r.pool.Query(ctx, `SELECT m.id,m.request_id,m.title,m.description,''::text,m.ordinal,
  m.is_published,m.created_at,m.updated_at,
  ARRAY(SELECT problem_id FROM material_problems WHERE material_id=m.id ORDER BY problem_id)
  FROM materials m WHERE ($1 OR m.is_published)
  AND ($2::uuid='00000000-0000-0000-0000-000000000000' OR EXISTS (
   SELECT 1 FROM material_problems WHERE material_id=m.id AND problem_id=$2))
  AND ($3::uuid='00000000-0000-0000-0000-000000000000' OR (m.ordinal,m.id)>($4,$3))
  ORDER BY m.ordinal,m.id LIMIT $5`, filter.All, filter.ProblemID, filter.After, filter.Ordinal, filter.Limit)
	if err != nil {
		return nil, fmt.Errorf("list materials: %w", err)
	}
	materials, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Material, error) { return scanMaterial(row) })
	if err != nil {
		return nil, fmt.Errorf("collect materials: %w", err)
	}
	return materials, nil
}

// Delete удаляет материал и каскадно его связи и метаданные вложений.
func (r *Repository) Delete(ctx context.Context, id uuid.UUID) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	tag, err := r.pool.Exec(ctx, `DELETE FROM materials WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("delete material: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
