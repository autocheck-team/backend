package billing

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/autocheck-team/backend/internal/auth"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

type providerStub struct {
	payment ProviderPayment
	err     error
	creates int
	gets    int
}

func (p *providerStub) Create(context.Context, Payment) (ProviderPayment, error) {
	p.creates++
	return p.payment, p.err
}

func (p *providerStub) Get(context.Context, string) (ProviderPayment, error) {
	p.gets++
	return p.payment, p.err
}

type storeStub struct {
	Store
	payment  Payment
	limits   Limits
	pending  []Payment
	applied  []ProviderPayment
	attached int
}

func (s *storeStub) Limits(context.Context, uuid.UUID) (Limits, error) { return s.limits, nil }
func (s *storeStub) Prepare(context.Context, uuid.UUID, uuid.UUID, string, string) (Payment, error) {
	return s.payment, nil
}
func (s *storeStub) ByProvider(context.Context, string) (Payment, error) { return s.payment, nil }
func (s *storeStub) Attach(_ context.Context, _ Payment, p ProviderPayment) error {
	s.attached++
	s.payment.ProviderID = &p.ID
	return nil
}

func (s *storeStub) Apply(_ context.Context, _ Payment, p ProviderPayment) error {
	s.applied = append(s.applied, p)
	return nil
}
func (s *storeStub) Pending(context.Context) ([]Payment, error) { return s.pending, nil }

func billingLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func subscriberContext(t *testing.T, id uuid.UUID) context.Context {
	t.Helper()
	return auth.WithPrincipal(t.Context(), auth.Principal{ID: id, Roles: []auth.Role{auth.RoleStudent}})
}

func TestDisabledBilling(t *testing.T) {
	t.Parallel()
	store := &storeStub{limits: Limits{Plan: Plan{Code: "free", CompilationLimit: 1, MonthlyTestRunLimit: 100}, Status: "free", Used: 100}}
	service := NewService(store, nil, billingLogger())
	ctx := subscriberContext(t, uuid.New())
	if _, err := service.CheckoutSubscription(ctx, &v1.CheckoutSubscriptionRequest{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("checkout=%v", err)
	}
	response, err := service.GetMySubscription(ctx, &v1.GetMySubscriptionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetCheckoutEnabled() || response.GetPlan().GetCode() != "free" || response.GetTestRunsRemaining() != 0 {
		t.Fatalf("limits=%v", response)
	}
	if err := service.SyncPending(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetMySubscription(t.Context(), &v1.GetMySubscriptionRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatal("anonymous subscription allowed")
	}
}

func TestWebhookUsesProviderState(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"pending", "succeeded", "canceled"} {
		t.Run(state, func(t *testing.T) {
			payment := testPayment()
			provider := providerFixture(payment, state)
			payment.ProviderID = &provider.ID
			store := &storeStub{payment: payment}
			client := &providerStub{payment: provider}
			service := NewService(store, client, billingLogger())
			_, err := service.YookassaWebhook(t.Context(), &v1.YookassaWebhookRequest{Event: "payment.succeeded", Object: &v1.YookassaWebhookObject{Id: provider.ID}})
			if err != nil {
				t.Fatal(err)
			}
			if client.gets != 1 || len(store.applied) != 1 || store.applied[0].Status != state {
				t.Fatal("webhook trusted event instead of API")
			}
		})
	}
}

func TestPendingRecovery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		attached      bool
		expired       bool
		providerError bool
	}{
		{name: "create_lost_response"}, {name: "refresh_pending", attached: true}, {name: "provider_failure", providerError: true}, {name: "expired_idempotency", expired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payment := testPayment()
			provider := providerFixture(payment, "succeeded")
			if tc.attached {
				payment.ProviderID = &provider.ID
			}
			if tc.expired {
				payment.CreatedAt = time.Now().Add(-25 * time.Hour)
			}
			store := &storeStub{payment: payment, pending: []Payment{payment}}
			client := &providerStub{payment: provider}
			if tc.providerError {
				client.err = errors.New("offline")
			}
			service := NewService(store, client, billingLogger())
			err := service.SyncPending(t.Context())
			if (err != nil) != (tc.expired || tc.providerError) {
				t.Fatalf("sync=%v", err)
			}
			if tc.expired || tc.providerError {
				if len(store.applied) != 0 {
					t.Fatal("failed payment applied")
				}
				return
			}
			if len(store.applied) != 1 || client.gets != 1 {
				t.Fatal("recovered payment was not verified with GET")
			}
			if !tc.attached && (client.creates != 1 || store.attached != 1) {
				t.Fatal("lost response not recovered")
			}
		})
	}
}

func TestCheckoutValidation(t *testing.T) {
	t.Parallel()
	payment := testPayment()
	for _, tc := range []struct {
		name      string
		returnURL string
		requestID string
		want      codes.Code
	}{
		{"relative_url", "/return", payment.RequestID.String(), codes.InvalidArgument},
		{"unsafe_scheme", "javascript:alert(1)", payment.RequestID.String(), codes.InvalidArgument},
		{"invalid_key", payment.ReturnURL, "bad", codes.InvalidArgument},
		{"key_reuse", "https://different.example", payment.RequestID.String(), codes.AlreadyExists},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := NewService(&storeStub{payment: payment}, &providerStub{}, billingLogger())
			_, err := service.CheckoutSubscription(subscriberContext(t, payment.UserID), &v1.CheckoutSubscriptionRequest{PlanCode: "pro", ReturnUrl: tc.returnURL, RequestId: tc.requestID})
			if status.Code(err) != tc.want {
				t.Fatalf("checkout=%v", err)
			}
		})
	}
}
