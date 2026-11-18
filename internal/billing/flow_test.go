package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

type checkoutProvider struct{ payment ProviderPayment }

func (p *checkoutProvider) Create(_ context.Context, payment Payment) (ProviderPayment, error) {
	p.payment = providerFixture(payment, "pending")
	return p.payment, nil
}

func (p *checkoutProvider) Get(context.Context, string) (ProviderPayment, error) {
	return p.payment, nil
}

func TestSubscriptionFlow(t *testing.T) {
	pool := billingDB(t)
	repo := NewRepository(pool)
	userID := seedSubscriber(t, pool)
	provider := &checkoutProvider{}
	service := NewService(repo, provider, billingLogger())
	ctx := subscriberContext(t, userID)
	plans, err := service.ListPlans(ctx, &v1.ListPlansRequest{})
	if err != nil || len(plans.GetPlans()) != 2 {
		t.Fatalf("catalogue: %v", err)
	}
	request := &v1.CheckoutSubscriptionRequest{PlanCode: "pro", ReturnUrl: "https://example.com", RequestId: uuid.NewString()}
	first, err := service.CheckoutSubscription(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CheckoutSubscription(ctx, request)
	if err != nil || first.GetPayment().GetId() != second.GetPayment().GetId() {
		t.Fatalf("retry: %v", err)
	}
	if first.GetConfirmationUrl() == "" {
		t.Fatal("missing redirect URL")
	}
	// Уведомления на локалке нет: pending успешно завершается фоновой сверкой.
	provider.payment.Status = "succeeded"
	provider.payment.Paid = true
	if err := service.SyncPending(ctx); err != nil {
		t.Fatal(err)
	}
	limits, err := service.GetMySubscription(ctx, &v1.GetMySubscriptionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if limits.GetPlan().GetCode() != "pro" || limits.GetStartTime() == nil || limits.GetEndTime() == nil || limits.GetCompilationsRemaining() != -1 {
		t.Fatalf("subscription=%v", limits)
	}
	// Повторный webhook после фонового подтверждения не добавляет ещё один период.
	if _, err := service.YookassaWebhook(ctx, &v1.YookassaWebhookRequest{Object: &v1.YookassaWebhookObject{Id: provider.payment.ID}}); err != nil {
		t.Fatal(err)
	}
	renewed, err := service.GetMySubscription(ctx, &v1.GetMySubscriptionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !renewed.GetEndTime().AsTime().Equal(limits.GetEndTime().AsTime()) {
		t.Fatal("webhook extended already settled payment")
	}
	// Несколько страниц и второй пользователь проверяют границы доступа.
	if _, err := repo.Prepare(ctx, userID, uuid.New(), "pro", "https://example.com/second"); err != nil {
		t.Fatal(err)
	}
	page, err := service.ListMyPayments(ctx, &v1.ListMyPaymentsRequest{PageSize: 1})
	if err != nil || len(page.GetPayments()) != 1 || page.GetNextPageToken() == "" {
		t.Fatalf("first page: %v", err)
	}
	next, err := service.ListMyPayments(ctx, &v1.ListMyPaymentsRequest{PageSize: 1, PageToken: page.GetNextPageToken()})
	if err != nil || len(next.GetPayments()) != 1 || next.GetPayments()[0].GetId() == page.GetPayments()[0].GetId() {
		t.Fatalf("next page: %v", err)
	}
	all, err := service.ListMyPayments(ctx, &v1.ListMyPaymentsRequest{})
	if err != nil || len(all.GetPayments()) != 2 {
		t.Fatalf("default page: %v", err)
	}
	other, err := service.ListMyPayments(subscriberContext(t, seedSubscriber(t, pool)), &v1.ListMyPaymentsRequest{})
	if err != nil || len(other.GetPayments()) != 0 {
		t.Fatal("payments owner isolation failed")
	}
	for _, request := range []*v1.ListMyPaymentsRequest{{PageToken: "bad"}, {PageSize: 101}, {PageSize: -1}} {
		if _, err := service.ListMyPayments(ctx, request); status.Code(err) != codes.InvalidArgument {
			t.Fatal("bad pagination accepted")
		}
	}
	if _, err := service.ListMyPayments(t.Context(), &v1.ListMyPaymentsRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatal("anonymous payment access")
	}
	if _, err := service.CheckoutSubscription(ctx, &v1.CheckoutSubscriptionRequest{PlanCode: "free", ReturnUrl: "https://example.com", RequestId: uuid.NewString()}); status.Code(err) != codes.InvalidArgument {
		t.Fatal("free checkout accepted")
	}
	pending, err := repo.Pending(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending queue: count=%d err=%v", len(pending), err)
	}
	again, err := repo.Pending(ctx)
	if err != nil || len(again) != 0 {
		t.Fatal("pending backoff ignored")
	}
}

func TestLimitsInTransaction(t *testing.T) {
	pool := billingDB(t)
	userID := seedSubscriber(t, pool)
	problemID := uuid.New()
	if _, err := pool.Exec(t.Context(), `INSERT INTO problems (id,author_id,slug,title,statement,time_limit_ms,memory_limit_kb)
  VALUES ($1,$2,'limits','Limits','test',1000,65536)`, problemID, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO test_cases (problem_id,ordinal,input_key,expected_key,is_ready) VALUES ($1,1,'in','out',true)`, problemID); err != nil {
		t.Fatal(err)
	}
	check := func(languageID string) (int32, error) {
		t.Helper()
		var budget int32
		err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(t.Context(), `SELECT id FROM users WHERE id=$1 FOR UPDATE`, userID); err != nil {
				return err
			}
			var err error
			budget, err = CheckSubmission(t.Context(), tx, userID, problemID, languageID)
			return err
		})
		return budget, err
	}
	if budget, err := check("go1.27"); err != nil || budget != 1 {
		t.Fatalf("initial budget=%d err=%v", budget, err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO submissions (problem_id,user_id,language_id,source_key,source_size)
  VALUES ($1,$2,'go1.27','source',1)`, problemID, userID); err != nil {
		t.Fatal(err)
	}
	_, err := check("go1.27")
	var limit *LimitError
	if !errors.As(err, &limit) || limit.RetryAfter <= 0 {
		t.Fatalf("compilation quota: %v", err)
	}
	if _, err := check("python3.14"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO submissions (problem_id,user_id,language_id,source_key,source_size,test_run_budget)
  VALUES ($1,$2,'python3.14','source',1,100)`, problemID, userID); err != nil {
		t.Fatal(err)
	}
	_, err = check("python3.14")
	if !errors.As(err, &limit) || limit.RetryAfter != 0 {
		t.Fatalf("monthly quota: %v", err)
	}
}

func TestSyncLifecycle(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			store := &storeStub{}
			var provider Provider
			if enabled {
				provider = &providerStub{}
			}
			service := NewService(store, provider, billingLogger())
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- service.RunSync(ctx, time.Millisecond) }()
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("sync did not stop")
			}
		})
	}
}
