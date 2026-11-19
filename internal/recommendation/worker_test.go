package recommendation

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
	"github.com/google/uuid"
)

func TestRefreshOnlyChanged(t *testing.T) {
	// arrange
	f := setup(t)
	var calls atomic.Int32
	f.svc.model = provider(t, embeddingHandler(t, &calls))
	// act
	if err := f.svc.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	// assert
	if calls.Load() != 1 {
		t.Fatal("unchanged catalog embedded again")
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM problem_embeddings WHERE embedding IS NOT NULL`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	var sumUpdated time.Time
	if err := f.pool.QueryRow(t.Context(), `SELECT updated_at FROM problem_embeddings WHERE problem_id=$1`, f.sum).Scan(&sumUpdated); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE problems SET statement='Графы: обход в глубину' WHERE id=$1`, f.graph); err != nil {
		t.Fatal(err)
	}
	response := f.recommend(t, &v1.RecommendProblemsRequest{Goal: "графы"})
	if !response.IsTextFallback {
		t.Fatal("stale vector used")
	}
	if err := f.svc.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	var unchanged time.Time
	if err := f.pool.QueryRow(t.Context(), `SELECT updated_at FROM problem_embeddings WHERE problem_id=$1`, f.sum).Scan(&unchanged); err != nil || !unchanged.Equal(sumUpdated) {
		t.Fatal("unchanged problem rewritten")
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE problems SET title='Графы: поиск в ширину' WHERE id=$1`, f.graph); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.svc.cfg.EmbeddingModel = "other-model"
	if err := f.svc.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 {
		t.Fatalf("refresh calls=%d", calls.Load())
	}
	items, err := f.repo.catalog(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Hash != item.hash() || item.Model != "other-model" || len(item.Vector) != 2 {
			t.Fatal("invalid persisted embedding")
		}
	}
	if _, err := f.pool.Exec(t.Context(), `DELETE FROM problems WHERE id=$1`, f.graph); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM problem_embeddings WHERE problem_id=$1`, f.graph).Scan(&count); err != nil || count != 0 {
		t.Fatal("embedding did not cascade")
	}
}

func TestEmbeddingLeaseAndStaleWrite(t *testing.T) {
	// arrange
	f := setup(t)
	items, err := f.repo.catalog(t.Context(), "go1.27")
	if err != nil || len(items) != 1 {
		t.Fatal(err)
	}
	item := items[0]
	token, claimed, err := f.repo.claim(t.Context(), item, "test-embedding", time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	// act
	if _, claimed, err := f.repo.claim(t.Context(), item, "test-embedding", time.Minute); err != nil || claimed {
		t.Fatal("duplicate claim")
	}
	if err := f.repo.complete(t.Context(), item, uuid.New(), []float64{1, 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE problems SET statement='новое условие' WHERE id=$1`, item.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.complete(t.Context(), item, token, []float64{1, 0}); err != nil {
		t.Fatal(err)
	}
	// assert
	var empty bool
	if err := f.pool.QueryRow(t.Context(), `SELECT embedding IS NULL FROM problem_embeddings WHERE problem_id=$1`, item.ID).Scan(&empty); err != nil || !empty {
		t.Fatal("stale result saved")
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE problem_embeddings SET lease_until=now()-interval '1 second' WHERE problem_id=$1`, item.ID); err != nil {
		t.Fatal(err)
	}
	items, err = f.repo.catalog(t.Context(), "go1.27")
	if err != nil {
		t.Fatal(err)
	}
	replacement, claimed, err := f.repo.claim(t.Context(), items[0], "test-embedding", time.Minute)
	if err != nil || !claimed || replacement == token {
		t.Fatal("expired lease did not recover")
	}
}

func TestRunRefreshAndCancellation(t *testing.T) {
	// arrange
	f := setup(t)
	var calls atomic.Int32
	f.svc.model = provider(t, embeddingHandler(t, &calls))
	f.svc.cfg.RefreshInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	failures := make(chan error, 10)
	// act
	go func() { defer close(done); f.svc.Run(ctx, func(err error) { failures <- err }) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var count int
		if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM problem_embeddings WHERE embedding IS NOT NULL`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background refresh timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	// assert
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker ignored cancellation")
	}
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if calls.Load() != 1 {
		t.Fatal("unexpected repeated embedding")
	}
}

func TestRefreshFailureRecovery(t *testing.T) {
	for _, test := range []struct{ name, body string }{
		{"missing", `{"data":[]}`},
		{"zero", `{"data":[{"index":0,"embedding":[0,0]},{"index":1,"embedding":[0,0]}]}`},
		{"dimensions", `{"data":[{"index":0,"embedding":[1]},{"index":1,"embedding":[1,2]}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			// arrange
			f := setup(t)
			f.svc.model = provider(t, func(w http.ResponseWriter, _ *http.Request) {
				if _, err := w.Write([]byte(test.body)); err != nil {
					t.Error(err)
				}
			})
			// act
			if err := f.svc.Refresh(t.Context()); err == nil {
				t.Fatal("invalid embedding accepted")
			}
			// assert
			if !f.recommend(t, &v1.RecommendProblemsRequest{Goal: "графы"}).IsTextFallback {
				t.Fatal("failed refresh did not fall back")
			}
			if _, err := f.pool.Exec(t.Context(), `UPDATE problem_embeddings SET lease_until=now()-interval '1 second'`); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			f.svc.model = provider(t, embeddingHandler(t, &calls))
			if err := f.svc.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 {
				t.Fatal("failed embedding did not recover")
			}
		})
	}
}
