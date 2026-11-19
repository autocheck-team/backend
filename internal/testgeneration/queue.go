package testgeneration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Claim захватывает queued или просроченную running генерацию; токен защищает от старого воркера.
func (r *Repository) Claim(ctx context.Context) (Job, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	job := Job{Token: uuid.New()}
	var data []byte
	err := r.pool.QueryRow(ctx, `WITH ready AS (
 SELECT id FROM test_generations WHERE status IN (1,2) AND (lease_until IS NULL OR lease_until<now())
 ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1)
 UPDATE test_generations g SET status=2,lease_token=$1,lease_until=now()+interval '60 seconds',attempts=attempts+1,updated_at=now()
 FROM ready WHERE g.id=ready.id RETURNING g.id,g.task,g.attempts`, job.Token).Scan(&job.ID, &data, &job.Attempts)
	if err != nil {
		return job, fmt.Errorf("claim test generation: %w", err)
	}
	if err := json.Unmarshal(data, &job.Task); err != nil {
		return job, fmt.Errorf("decode generation task: %w", err)
	}
	return job, nil
}

// Heartbeat продлевает только ещё действующую аренду.
func (r *Repository) Heartbeat(ctx context.Context, j Job) error {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	tag, err := r.pool.Exec(ctx, `UPDATE test_generations SET lease_until=now()+interval '60 seconds' WHERE id=$1 AND lease_token=$2 AND status=2 AND lease_until>now()`, j.ID, j.Token)
	if err != nil {
		return fmt.Errorf("renew generation lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

// Release возвращает работу после остановки процесса, не расходуя попытку.
func (r *Repository) Release(ctx context.Context, j Job) error {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	_, err := r.pool.Exec(ctx, `UPDATE test_generations SET status=1,lease_token=NULL,lease_until=NULL,attempts=GREATEST(0,attempts-1),updated_at=now() WHERE id=$1 AND lease_token=$2 AND status=2`, j.ID, j.Token)
	if err != nil {
		return fmt.Errorf("release generation: %w", err)
	}
	return nil
}

// Finish сохраняет все черновики и состояние атомарно; текст ошибки задаёт сценарий.
func (r *Repository) Finish(ctx context.Context, j Job, drafts []Draft, failure string) error {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var id uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id FROM test_generations WHERE id=$1 AND lease_token=$2 AND status=2 AND lease_until>now() FOR UPDATE`, j.ID, j.Token).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return fmt.Errorf("lock generation lease: %w", err)
		}
		batch := &pgx.Batch{}
		for i, draft := range drafts {
			batch.Queue(`INSERT INTO generated_tests (generation_id,ordinal,input,expected,explanation,rejection_reason) VALUES ($1,$2,$3,$4,$5,$6)`, j.ID, i+1, draft.Input, draft.Expected, draft.Explanation, draft.RejectionReason)
		}
		if len(drafts) > 0 {
			result := tx.SendBatch(ctx, batch)
			if err := result.Close(); err != nil {
				return fmt.Errorf("save generation drafts: %w", err)
			}
		}
		state := 3
		if failure != "" {
			state = 4
		}
		// Эталон после завершения не нужен: уменьшаем срок хранения исходника автора.
		_, err = tx.Exec(ctx, `UPDATE test_generations SET status=$3,error=$4,task=task-'Source',lease_token=NULL,lease_until=NULL,updated_at=now() WHERE id=$1 AND lease_token=$2`, j.ID, j.Token, state, failure)
		if err != nil {
			return fmt.Errorf("complete generation: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("finish test generation: %w", err)
	}
	return nil
}
