package sandbox

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/autocheck-team/backend/internal/db"
)

// Образы языков из таблицы languages должны существовать в реестре: иначе посылки
// на этом языке упадут при проверке. Проверяется только манифест, без скачивания.
func TestLanguageImagesExist(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	sb, err := New("")
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	defer func() { _ = sb.Close() }()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if _, err := sb.cli.Ping(ctx, client.PingOptions{}); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	rows, err := pool.Query(ctx, `SELECT id, image FROM languages WHERE is_active`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, image string
		if err := rows.Scan(&id, &image); err != nil {
			t.Fatal(err)
		}
		t.Run(id, func(t *testing.T) {
			if _, err := sb.cli.DistributionInspect(ctx, image, client.DistributionInspectOptions{}); err != nil {
				t.Fatalf("image %s: %v", image, err)
			}
		})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
