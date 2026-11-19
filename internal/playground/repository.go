package playground

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository хранит проекты и очередь в том же Postgres, что и посылки.
type Repository struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

// NewRepository задаёт пул и таймаут каждой операции хранилища.
func NewRepository(pool *pgxpool.Pool, timeout time.Duration) *Repository {
	return &Repository{pool: pool, timeout: timeout}
}

const projectColumns = `id, user_id, client_request_id, title, language_id, files, created_at, updated_at`

func scanProject(row pgx.Row) (Project, error) {
	var project Project
	err := row.Scan(&project.ID, &project.UserID, &project.RequestID, &project.Title,
		&project.LanguageID, &project.Files, &project.CreatedAt, &project.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("read project: %w", err)
	}
	return project, nil
}

func language(ctx context.Context, tx pgx.Tx, id string, files []File) (Language, error) {
	var result Language
	err := tx.QueryRow(ctx, `SELECT image, source_file, compile_cmd, run_cmd, compile_env
 FROM languages WHERE id=$1 AND is_active`, id).Scan(&result.Image, &result.SourceFile,
		&result.CompileCmd, &result.RunCmd, &result.CompileEnv)
	if errors.Is(err, pgx.ErrNoRows) {
		return Language{}, ErrLanguage
	}
	if err != nil {
		return Language{}, fmt.Errorf("read playground language: %w", err)
	}
	if err := validateFiles(files, result.SourceFile); err != nil {
		return Language{}, err
	}
	return result, nil
}

// Create сериализует лимит и идемпотентность блокировкой владельца.
func (r *Repository) Create(ctx context.Context, project Project) (Project, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	var result Project
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, project.UserID); err != nil {
			return fmt.Errorf("lock project owner: %w", err)
		}
		existing, err := scanProject(tx.QueryRow(ctx, `SELECT `+projectColumns+`
   FROM playground_projects WHERE user_id=$1 AND client_request_id=$2`, project.UserID, project.RequestID))
		if err == nil {
			result = existing
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM playground_projects
   WHERE user_id=$1 AND deleted_at IS NULL`, project.UserID).Scan(&count); err != nil {
			return fmt.Errorf("count projects: %w", err)
		}
		if count >= maxProjects {
			return ErrProjectLimit
		}
		if _, err := language(ctx, tx, project.LanguageID, project.Files); err != nil {
			return err
		}
		result, err = scanProject(tx.QueryRow(ctx, `INSERT INTO playground_projects
   (user_id, client_request_id, title, language_id, files) VALUES ($1,$2,$3,$4,$5) RETURNING `+projectColumns,
			project.UserID, project.RequestID, project.Title, project.LanguageID, project.Files))
		return err
	})
	if err != nil {
		return Project{}, fmt.Errorf("create project: %w", err)
	}
	return result, nil
}

// Get читает только неудалённый проект; права чтения проверяет сценарий.
func (r *Repository) Get(ctx context.Context, id uuid.UUID) (Project, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return scanProject(r.pool.QueryRow(ctx, `SELECT `+projectColumns+`
 FROM playground_projects WHERE id=$1 AND deleted_at IS NULL`, id))
}

// List возвращает только проекты владельца по убыванию UUIDv7.
func (r *Repository) List(ctx context.Context, userID, after uuid.UUID, limit int) ([]Project, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	rows, err := r.pool.Query(ctx, `SELECT `+projectColumns+` FROM playground_projects
 WHERE user_id=$1 AND deleted_at IS NULL AND ($2::uuid='00000000-0000-0000-0000-000000000000' OR id<$2)
 ORDER BY id DESC LIMIT $3`, userID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	projects, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Project, error) { return scanProject(row) })
	if err != nil {
		return nil, fmt.Errorf("collect projects: %w", err)
	}
	return projects, nil
}

// Update заменяет файлы атомарно; администратор не получает права редактирования чужого проекта.
func (r *Repository) Update(ctx context.Context, project Project) (Project, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	var result Project
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := scanProject(tx.QueryRow(ctx, `SELECT `+projectColumns+` FROM playground_projects
   WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL FOR UPDATE`, project.ID, project.UserID)); err != nil {
			return err
		}
		if _, err := language(ctx, tx, project.LanguageID, project.Files); err != nil {
			return err
		}
		var err error
		result, err = scanProject(tx.QueryRow(ctx, `UPDATE playground_projects
   SET title=$3, language_id=$4, files=$5, updated_at=clock_timestamp()
   WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL RETURNING `+projectColumns,
			project.ID, project.UserID, project.Title, project.LanguageID, project.Files))
		return err
	})
	if err != nil {
		return Project{}, fmt.Errorf("update project: %w", err)
	}
	return result, nil
}

// Delete скрывает проект, сохраняя снимки и расход компиляций.
func (r *Repository) Delete(ctx context.Context, id, userID uuid.UUID) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	tag, err := r.pool.Exec(ctx, `UPDATE playground_projects SET deleted_at=COALESCE(deleted_at,now()),
 updated_at=clock_timestamp() WHERE id=$1 AND user_id=$2`, id, userID)
	if err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
