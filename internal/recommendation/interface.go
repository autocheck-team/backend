package recommendation

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/autocheck-team/backend/internal/llm"
)

// Model использует общий клиент llm и не получает исходники решений.
type Model interface {
	Enabled() bool
	Embed(context.Context, []string, llm.Options) ([][]float64, error)
	Chat(context.Context, []llm.Message, llm.Options) (json.RawMessage, error)
}

type repository interface {
	catalog(context.Context, string) ([]candidate, error)
	search(context.Context, string, string, int) ([]match, error)
	reserve(context.Context, uuid.UUID, int) (time.Duration, error)
	claim(context.Context, candidate, string, time.Duration) (uuid.UUID, bool, error)
	complete(context.Context, candidate, uuid.UUID, []float64) error
	visible(context.Context, []uuid.UUID, string) (map[uuid.UUID]bool, error)
}
