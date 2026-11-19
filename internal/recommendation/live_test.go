package recommendation

import (
	"os"
	"testing"
	"time"

	"github.com/autocheck-team/backend/internal/llm"
)

// TestOpenRouterEmbeddingsLive проверяет один batch без повторов, ключи и тексты не логируются.
func TestOpenRouterEmbeddingsLive(t *testing.T) {
	// arrange
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		key = os.Getenv("LLM_API_KEY")
	}
	if key == "" {
		t.Skip("OPENROUTER_API_KEY/LLM_API_KEY not set")
	}
	model, err := llm.New(llm.Config{APIKey: key, EmbeddingModel: os.Getenv("LLM_EMBEDDING_MODEL"), Timeout: 2 * time.Minute, Retries: 0})
	if err != nil {
		t.Fatal(err)
	}
	// act
	vectors, err := model.Embed(t.Context(), []string{"Learn shortest paths in graphs", "Find the shortest path in a graph"}, llm.Options{})
	// assert
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 2 {
		t.Fatal("missing live embeddings")
	}
	if score, ok := cosine(vectors[0], vectors[1]); !ok || score < -1 || score > 1 {
		t.Fatal("invalid live vectors")
	}
}
