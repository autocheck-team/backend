package recommendation

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

func TestExplanationFailureDoesNotLoseRanking(t *testing.T) {
	for _, test := range []struct {
		name, body string
		code       int
	}{
		{"rate_limit", "", 429},
		{"unavailable", "", 503},
		{"invalid_json", "broken", 200},
		{"too_long", `{"explanation":"` + strings.Repeat("я", 501) + `"}`, 200},
		{"code", "{\"explanation\":\"```go code```\"}", 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			// arrange
			f := setup(t)
			var calls atomic.Int32
			embedding := embeddingHandler(t, &calls)
			f.svc.model = provider(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/embeddings" {
					embedding(w, r)
					return
				}
				w.WriteHeader(test.code)
				if test.code == 200 {
					encode(t, w, map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": test.body}}}})
				}
			})
			if err := f.svc.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			// act
			response := f.recommend(t, &v1.RecommendProblemsRequest{Goal: "графы", Limit: 1})
			// assert
			if response.IsTextFallback || response.Explanation != "" || len(response.Recommendations) != 1 || response.Recommendations[0].ProblemId != f.graph.String() {
				t.Fatalf("ranking lost: %v", response)
			}
		})
	}
}

func TestUnpublishedDuringExplanation(t *testing.T) {
	// arrange
	f := setup(t)
	var calls atomic.Int32
	embedding := embeddingHandler(t, &calls)
	f.svc.model = provider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/embeddings" {
			embedding(w, r)
			return
		}
		if _, err := f.pool.Exec(r.Context(), `UPDATE problems SET is_published=false WHERE id=$1`, f.graph); err != nil {
			t.Error(err)
		}
		encode(t, w, map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": `{"explanation":"Графы помогут научиться обходам."}`}}}})
	})
	if err := f.svc.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	// act
	response := f.recommend(t, &v1.RecommendProblemsRequest{Goal: "графы", Limit: 1})
	// assert
	if len(response.Recommendations) != 0 || response.Explanation != "" {
		t.Fatal("unpublished recommendation leaked")
	}
}
