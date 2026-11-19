package recommendation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/autocheck-team/backend/internal/llm"
)

// Refresh пересчитывает не более 16 изменённых заданий одним batch-вызовом модели.
// Аренда живёт дольше deadline модели; транзакции БД не охватывают сетевой запрос.
func (s *Service) Refresh(ctx context.Context) error {
	if !s.model.Enabled() {
		return nil
	}
	items, err := s.repo.catalog(ctx, "")
	if err != nil {
		return fmt.Errorf("load embedding sources: %w", err)
	}
	pending := make([]candidate, 0, 16)
	tokens := make([]uuid.UUID, 0, 16)
	texts := make([]string, 0, 16)
	for _, item := range items {
		if item.Hash == item.hash() && item.Model == s.cfg.EmbeddingModel && validVector(item.Vector) {
			continue
		}
		token, claimed, err := s.repo.claim(ctx, item, s.cfg.EmbeddingModel, s.cfg.ModelTimeout+s.cfg.RefreshInterval)
		if err != nil {
			return fmt.Errorf("lease embedding source: %w", err)
		}
		if !claimed {
			continue
		}
		pending = append(pending, item)
		tokens = append(tokens, token)
		texts = append(texts, item.text())
		if len(pending) == 16 {
			break
		}
	}
	if len(pending) == 0 {
		return nil
	}
	modelCtx, cancel := context.WithTimeout(ctx, s.cfg.ModelTimeout)
	vectors, err := s.model.Embed(modelCtx, texts, llm.Options{})
	cancel()
	if err != nil {
		return fmt.Errorf("embed published problems: %w", err)
	}
	if len(vectors) != len(pending) {
		return errors.New("incomplete problem embeddings")
	}
	dimensions := len(vectors[0])
	for _, vector := range vectors {
		if !validVector(vector) || len(vector) != dimensions {
			return errors.New("invalid problem embedding")
		}
	}
	for i, item := range pending {
		if err := s.repo.complete(ctx, item, tokens[i], vectors[i]); err != nil {
			return fmt.Errorf("store refreshed embedding: %w", err)
		}
	}
	return nil
}

// Run обновляет каталог при старте API и затем по таймеру до отмены контекста.
func (s *Service) Run(ctx context.Context, report func(error)) {
	if !s.model.Enabled() {
		return
	}
	ticker := time.NewTicker(s.cfg.RefreshInterval)
	defer ticker.Stop()
	for {
		if err := s.Refresh(ctx); err != nil && ctx.Err() == nil {
			report(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
