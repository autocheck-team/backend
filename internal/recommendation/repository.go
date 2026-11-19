// Package recommendation подбирает задания по эмбеддингам с текстовым резервным поиском.
package recommendation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type candidate struct {
	ID               uuid.UUID
	Title, Statement string
	Hash, Model      string
	Vector           []float64
}

func (c candidate) text() string { return c.Title + "\n" + c.Statement }

func (c candidate) hash() string {
	sum := sha256.Sum256([]byte(c.text()))
	return hex.EncodeToString(sum[:])
}

type match struct {
	ID    uuid.UUID
	Score float64
}

// Repository хранит векторы, аренды пересчёта и суточный расход в PostgreSQL.
type Repository struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

// NewRepository задаёт предельное время каждой операции БД.
func NewRepository(pool *pgxpool.Pool, timeout time.Duration) *Repository {
	return &Repository{pool: pool, timeout: timeout}
}

func (r *Repository) catalog(parent context.Context, language string) ([]candidate, error) {
	ctx, cancel := context.WithTimeout(parent, r.timeout)
	defer cancel()
	rows, err := r.pool.Query(ctx, `SELECT p.id, p.title, p.statement,
        COALESCE(e.content_hash, ''), COALESCE(e.model, ''), e.embedding
        FROM problems p LEFT JOIN problem_embeddings e ON e.problem_id=p.id
        WHERE p.is_published AND ($1='' OR EXISTS (
            SELECT 1 FROM problem_languages pl JOIN languages l ON l.id=pl.language_id
            WHERE pl.problem_id=p.id AND pl.language_id=$1 AND l.is_active)) ORDER BY p.id`, language)
	if err != nil {
		return nil, fmt.Errorf("read recommendation catalog: %w", err)
	}
	result, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var item candidate
		err := row.Scan(&item.ID, &item.Title, &item.Statement, &item.Hash, &item.Model, &item.Vector)
		return item, err
	})
	if err != nil {
		return nil, fmt.Errorf("decode recommendation catalog: %w", err)
	}
	return result, nil
}

func (r *Repository) search(parent context.Context, goal, language string, limit int) ([]match, error) {
	ctx, cancel := context.WithTimeout(parent, r.timeout)
	defer cancel()
	rows, err := r.pool.Query(ctx, `WITH terms AS (
        SELECT DISTINCT plainto_tsquery('russian', term) AS query
        FROM regexp_split_to_table($1, '\s+') AS term WHERE length(term)>1
    ), ranked AS (
        SELECT p.id, SUM(ts_rank_cd(to_tsvector('russian', p.title || ' ' || p.statement), t.query)) AS rank
        FROM problems p CROSS JOIN terms t
        WHERE p.is_published AND to_tsvector('russian', p.title || ' ' || p.statement) @@ t.query
        AND ($2='' OR EXISTS (SELECT 1 FROM problem_languages pl JOIN languages l ON l.id=pl.language_id
            WHERE pl.problem_id=p.id AND pl.language_id=$2 AND l.is_active)) GROUP BY p.id
    ) SELECT id, (rank/(1+rank))::double precision FROM ranked ORDER BY rank DESC, id LIMIT $3`, goal, language, limit)
	if err != nil {
		return nil, fmt.Errorf("search recommendation text: %w", err)
	}
	result, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (match, error) {
		var item match
		err := row.Scan(&item.ID, &item.Score)
		return item, err
	})
	if err != nil {
		return nil, fmt.Errorf("decode text recommendations: %w", err)
	}
	return result, nil
}

func (r *Repository) reserve(parent context.Context, userID uuid.UUID, limit int) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(parent, r.timeout)
	defer cancel()
	var requests int
	err := r.pool.QueryRow(ctx, `INSERT INTO recommendation_usage (user_id,usage_on,requests)
        VALUES ($1,(now() AT TIME ZONE 'UTC')::date,1)
        ON CONFLICT (user_id,usage_on) DO UPDATE SET requests=recommendation_usage.requests+1, updated_at=now()
        WHERE recommendation_usage.requests<$2 RETURNING requests`, userID, limit).Scan(&requests)
	if errors.Is(err, pgx.ErrNoRows) {
		var seconds float64
		err := r.pool.QueryRow(ctx, `SELECT EXTRACT(EPOCH FROM (
            ((now() AT TIME ZONE 'UTC')::date+1)::timestamp AT TIME ZONE 'UTC')-now())::double precision`).Scan(&seconds)
		if err != nil {
			return 0, fmt.Errorf("read quota reset time: %w", err)
		}
		return time.Duration(seconds * float64(time.Second)), nil
	}
	if err != nil {
		return 0, fmt.Errorf("reserve recommendation quota: %w", err)
	}
	return 0, nil
}

func (r *Repository) claim(parent context.Context, item candidate, model string, lease time.Duration) (uuid.UUID, bool, error) {
	ctx, cancel := context.WithTimeout(parent, r.timeout)
	defer cancel()
	token, err := uuid.NewRandom()
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("generate embedding lease token: %w", err)
	}
	tag, err := r.pool.Exec(ctx, `INSERT INTO problem_embeddings
        (problem_id,content_hash,model,lease_token,lease_until)
        SELECT id,$2,$3,$4,now()+make_interval(secs=>$5) FROM problems
        WHERE id=$1 AND is_published AND title=$6 AND statement=$7
        ON CONFLICT (problem_id) DO UPDATE SET content_hash=$2,model=$3,embedding=NULL,
            lease_token=$4,lease_until=now()+make_interval(secs=>$5),updated_at=now()
        WHERE problem_embeddings.lease_until<=now() AND
            (problem_embeddings.content_hash<>$2 OR problem_embeddings.model<>$3 OR problem_embeddings.embedding IS NULL)`,
		item.ID, item.hash(), model, token, lease.Seconds(), item.Title, item.Statement)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("claim problem embedding: %w", err)
	}
	return token, tag.RowsAffected() > 0, nil
}

func (r *Repository) complete(parent context.Context, item candidate, token uuid.UUID, vector []float64) error {
	ctx, cancel := context.WithTimeout(parent, r.timeout)
	defer cancel()
	_, err := r.pool.Exec(ctx, `UPDATE problem_embeddings e SET embedding=$3::real[],lease_until=now(),updated_at=now()
        WHERE e.problem_id=$1 AND e.lease_token=$2 AND EXISTS (
            SELECT 1 FROM problems p WHERE p.id=e.problem_id AND p.is_published AND p.title=$4 AND p.statement=$5)`,
		item.ID, token, vector, item.Title, item.Statement)
	if err != nil {
		return fmt.Errorf("save problem embedding: %w", err)
	}
	return nil
}

func (r *Repository) visible(parent context.Context, ids []uuid.UUID, language string) (map[uuid.UUID]bool, error) {
	ctx, cancel := context.WithTimeout(parent, r.timeout)
	defer cancel()
	rows, err := r.pool.Query(ctx, `SELECT p.id FROM problems p WHERE p.id=ANY($1::uuid[]) AND p.is_published
        AND ($2='' OR EXISTS (SELECT 1 FROM problem_languages pl JOIN languages l ON l.id=pl.language_id
            WHERE pl.problem_id=p.id AND pl.language_id=$2 AND l.is_active))`, ids, language)
	if err != nil {
		return nil, fmt.Errorf("check recommended problems: %w", err)
	}
	values, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("decode visible recommendations: %w", err)
	}
	result := make(map[uuid.UUID]bool, len(values))
	for _, id := range values {
		result[id] = true
	}
	return result, nil
}
