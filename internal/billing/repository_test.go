package billing

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/autocheck-team/backend/internal/db"
)

func billingDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	admin, err := db.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := "billing_" + uuid.New().String()[:8]
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`); err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func seedSubscriber(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(t.Context(), `INSERT INTO users (id,email,display_name) VALUES ($1,$2,'billing')`, id, id.String()+"@test.local"); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRepositoryActivation(t *testing.T) {
	pool := billingDB(t)
	repo := NewRepository(pool)
	userID := seedSubscriber(t, pool)
	requestID := uuid.New()
	payment, err := repo.Prepare(t.Context(), userID, requestID, "pro", "https://example.com/return")
	if err != nil {
		t.Fatal(err)
	}
	retry, err := repo.Prepare(t.Context(), userID, requestID, "pro", payment.ReturnURL)
	if err != nil {
		t.Fatal(err)
	}
	if payment.ID != retry.ID || payment.IdempotenceKey != retry.IdempotenceKey {
		t.Fatal("local idempotency failed")
	}
	provider := providerFixture(payment, "succeeded")
	if err := repo.Attach(t.Context(), payment, provider); err != nil {
		t.Fatal(err)
	}
	payment.ProviderID = &provider.ID
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			if err := repo.Apply(t.Context(), payment, provider); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	before, err := repo.Limits(t.Context(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Plan.Code != "pro" || before.EndsAt == nil || before.StartsAt == nil || before.EndsAt.Sub(*before.StartsAt) != 30*24*time.Hour {
		t.Fatalf("activation=%+v", before)
	}
	// Поздний pending не откатывает terminal-состояние.
	if err := repo.Apply(t.Context(), payment, providerFixture(payment, "pending")); err != nil {
		t.Fatal(err)
	}
	second, err := repo.Prepare(t.Context(), userID, uuid.New(), "pro", payment.ReturnURL)
	if err != nil {
		t.Fatal(err)
	}
	secondProvider := providerFixture(second, "succeeded")
	secondProvider.ID = "provider-456"
	if err := repo.Attach(t.Context(), second, secondProvider); err != nil {
		t.Fatal(err)
	}
	if err := repo.Apply(t.Context(), second, secondProvider); err != nil {
		t.Fatal(err)
	}
	after, err := repo.Limits(t.Context(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if after.EndsAt.Sub(*before.EndsAt) != 30*24*time.Hour {
		t.Fatal("renewal did not add exactly one period")
	}
	if after.RemainingTests() != -1 {
		t.Fatal("pro test quota should be unlimited")
	}
	if _, err := pool.Exec(t.Context(), `UPDATE subscriptions SET starts_at=now()-interval '60 days',ends_at=now()-interval '1 second' WHERE user_id=$1`, userID); err != nil {
		t.Fatal(err)
	}
	expired, err := repo.Limits(t.Context(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if expired.Plan.Code != "free" || expired.Status != "expired" {
		t.Fatalf("expiry=%+v", expired)
	}
	payments, err := repo.Payments(t.Context(), userID, uuid.Nil, 1)
	if err != nil || len(payments) != 1 {
		t.Fatalf("payments=%d err=%v", len(payments), err)
	}
	other := seedSubscriber(t, pool)
	payments, err = repo.Payments(t.Context(), other, uuid.Nil, 10)
	if err != nil || len(payments) != 0 {
		t.Fatal("payments leaked to another user")
	}
}

func TestRepositoryRejectsForgedTerms(t *testing.T) {
	pool := billingDB(t)
	repo := NewRepository(pool)
	userID := seedSubscriber(t, pool)
	payment, err := repo.Prepare(t.Context(), userID, uuid.New(), "pro", "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	provider := providerFixture(payment, "succeeded")
	if err := repo.Attach(t.Context(), payment, provider); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []string{"amount", "currency", "metadata", "unpaid", "id", "status"} {
		t.Run(tc, func(t *testing.T) {
			forged := providerFixture(payment, "succeeded")
			switch tc {
			case "amount":
				forged.Amount.Value = "0.01"
			case "currency":
				forged.Amount.Currency = "USD"
			case "metadata":
				forged.Metadata = nil
			case "unpaid":
				forged.Paid = false
			case "id":
				forged.ID = "different"
			case "status":
				forged.Status = "refunded"
			}
			if err := repo.Apply(t.Context(), payment, forged); err == nil {
				t.Fatal("forged payment accepted")
			}
		})
	}
	limits, err := repo.Limits(t.Context(), userID)
	if err != nil || limits.Plan.Code != "free" {
		t.Fatalf("forged activation: %v", err)
	}
	provider.Status = "canceled"
	provider.Paid = false
	if err := repo.Apply(t.Context(), payment, provider); err != nil {
		t.Fatal(err)
	}
	if err := repo.Apply(t.Context(), payment, providerFixture(payment, "succeeded")); err != nil {
		t.Fatal(err)
	}
	limits, err = repo.Limits(t.Context(), userID)
	if err != nil || limits.Plan.Code != "free" {
		t.Fatal("canceled payment activated")
	}
	if _, err := repo.ByProvider(t.Context(), "unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatal("expected missing payment")
	}
}
