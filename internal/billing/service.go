package billing

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/autocheck-team/backend/internal/auth"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

// Store объявлен у сценария, чтобы проверять сбои провайдера независимо от БД.
type Store interface {
	Plans(ctx context.Context) ([]Plan, error)
	Prepare(ctx context.Context, userID, requestID uuid.UUID, planCode, returnURL string) (Payment, error)
	ByProvider(ctx context.Context, id string) (Payment, error)
	Attach(ctx context.Context, payment Payment, provider ProviderPayment) error
	Apply(ctx context.Context, payment Payment, provider ProviderPayment) error
	Pending(ctx context.Context) ([]Payment, error)
	Payments(ctx context.Context, userID, after uuid.UUID, limit int) ([]Payment, error)
	Limits(ctx context.Context, userID uuid.UUID) (Limits, error)
}

// Service обслуживает BillingService и восстановление неопределённых оплат.
type Service struct {
	v1.UnimplementedBillingServiceServer
	repo     Store
	provider Provider
	log      *slog.Logger
}

// NewService собирает биллинг; nil provider отключает checkout, но не бесплатные квоты.
func NewService(repo Store, provider Provider, log *slog.Logger) *Service {
	return &Service{repo: repo, provider: provider, log: log}
}

// ListPlans возвращает публичный каталог тарифов.
func (s *Service) ListPlans(ctx context.Context, _ *v1.ListPlansRequest) (*v1.ListPlansResponse, error) {
	plans, err := s.repo.Plans(ctx)
	if err != nil {
		return nil, err
	}
	resp := &v1.ListPlansResponse{Plans: make([]*v1.Plan, 0, len(plans))}
	for _, plan := range plans {
		resp.Plans = append(resp.Plans, planProto(plan))
	}
	return resp, nil
}

// GetMySubscription показывает эффективный доступ даже при выключенной платёжке.
func (s *Service) GetMySubscription(ctx context.Context, _ *v1.GetMySubscriptionRequest) (*v1.GetMySubscriptionResponse, error) {
	user, err := auth.Require(ctx)
	if err != nil {
		return nil, err
	}
	limits, err := s.repo.Limits(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	resp := &v1.GetMySubscriptionResponse{
		Plan: planProto(limits.Plan), Status: limits.Status, TestRunsUsed: limits.Used,
		TestRunsRemaining: limits.RemainingTests(), CompilationsRemaining: -1, CompilationRetryAfterSeconds: int64(limits.RetryAfter / time.Second), CheckoutEnabled: s.provider != nil,
	}
	if limits.Plan.CompilationLimit > 0 {
		resp.CompilationsRemaining = max(0, int64(limits.Plan.CompilationLimit)-limits.Compilations)
	}
	if limits.StartsAt != nil {
		resp.StartTime = timestamppb.New(*limits.StartsAt)
	}
	if limits.EndsAt != nil {
		resp.EndTime = timestamppb.New(*limits.EndsAt)
	}
	return resp, nil
}

// CheckoutSubscription сохраняет намерение и возвращает страницу оплаты без активации по редиректу.
func (s *Service) CheckoutSubscription(ctx context.Context, req *v1.CheckoutSubscriptionRequest) (*v1.CheckoutSubscriptionResponse, error) {
	user, err := auth.Require(ctx)
	if err != nil {
		return nil, err
	}
	if s.provider == nil {
		return nil, status.Error(codes.Unavailable, "оформление подписки временно недоступно")
	}
	parsed, err := url.Parse(req.GetReturnUrl())
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, status.Error(codes.InvalidArgument, "return_url должен быть абсолютным HTTP(S) адресом")
	}
	requestID, err := uuid.Parse(req.GetRequestId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "некорректный request_id")
	}
	payment, err := s.repo.Prepare(ctx, user.ID, requestID, req.GetPlanCode(), req.GetReturnUrl())
	if errors.Is(err, ErrNotFound) {
		return nil, status.Error(codes.InvalidArgument, "платный тариф не найден")
	}
	if err != nil {
		return nil, err
	}
	if payment.PlanCode != req.GetPlanCode() || payment.ReturnURL != req.GetReturnUrl() {
		return nil, status.Error(codes.AlreadyExists, "request_id уже использован с другими параметрами")
	}
	if payment.ProviderID == nil {
		payment, err = s.create(ctx, payment)
		if err != nil {
			s.log.WarnContext(ctx, "payment creation deferred", "payment_id", payment.ID, "err", err)
			return nil, status.Error(codes.Unavailable, "платёж обрабатывается, повторите запрос с прежним request_id")
		}
	}
	return &v1.CheckoutSubscriptionResponse{Payment: paymentProto(payment), ConfirmationUrl: payment.ConfirmationURL}, nil
}

func (s *Service) create(ctx context.Context, payment Payment) (Payment, error) {
	// Ключ ЮKassa живёт 24 часа: неопределённый старый запрос нельзя повторять как новую оплату.
	if time.Since(payment.CreatedAt) > 23*time.Hour {
		return payment, errors.New("payment creation requires manual reconciliation after idempotency expiry")
	}
	provider, err := s.provider.Create(ctx, payment)
	if err != nil {
		return payment, fmt.Errorf("create provider payment: %w", err)
	}
	if err := validatePayment(payment, provider); err != nil {
		return payment, err
	}
	if err := s.repo.Attach(ctx, payment, provider); err != nil {
		return payment, err
	}
	payment.ProviderID = &provider.ID
	payment.ConfirmationURL = provider.Confirmation.URL
	// Конечный статус принимается только из GET, включая редкий мгновенно завершённый платёж.
	if provider.Status != "pending" {
		if err := s.refresh(ctx, payment); err != nil {
			return payment, err
		}
		current, err := s.repo.ByProvider(ctx, *payment.ProviderID)
		if err != nil {
			return payment, err
		}
		return current, nil
	}
	return payment, nil
}

func (s *Service) refresh(ctx context.Context, payment Payment) error {
	provider, err := s.provider.Get(ctx, *payment.ProviderID)
	if err != nil {
		return fmt.Errorf("get provider payment: %w", err)
	}
	if err := validatePayment(payment, provider); err != nil {
		return err
	}
	return s.repo.Apply(ctx, payment, provider)
}

// YookassaWebhook не использует event и поля объекта как доказательство оплаты.
func (s *Service) YookassaWebhook(ctx context.Context, req *v1.YookassaWebhookRequest) (*v1.YookassaWebhookResponse, error) {
	if s.provider == nil {
		return nil, status.Error(codes.Unavailable, "биллинг выключен")
	}
	payment, err := s.repo.ByProvider(ctx, req.GetObject().GetId())
	if errors.Is(err, ErrNotFound) {
		return &v1.YookassaWebhookResponse{}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := s.refresh(ctx, payment); err != nil {
		return nil, err
	}
	return &v1.YookassaWebhookResponse{}, nil
}

// SyncPending восстанавливает также создание платежа, ответ которого потерялся до записи provider_id.
func (s *Service) SyncPending(ctx context.Context) error {
	if s.provider == nil {
		return nil
	}
	payments, err := s.repo.Pending(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, payment := range payments {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if payment.ProviderID == nil {
			_, err = s.create(ctx, payment)
		} else {
			err = s.refresh(ctx, payment)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("sync payment %s: %w", payment.ID, err))
		}
	}
	return errors.Join(errs...)
}

// RunSync работает до отмены API; вызывающий ожидает завершения через errgroup.
func (s *Service) RunSync(ctx context.Context, interval time.Duration) error {
	if s.provider == nil {
		<-ctx.Done()
		return nil
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := s.SyncPending(ctx); err != nil && ctx.Err() == nil {
			s.log.WarnContext(ctx, "payment sync failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// ListMyPayments ограничивает выборку владельцем и непрозрачным токеном страницы.
func (s *Service) ListMyPayments(ctx context.Context, req *v1.ListMyPaymentsRequest) (*v1.ListMyPaymentsResponse, error) {
	user, err := auth.Require(ctx)
	if err != nil {
		return nil, err
	}
	limit := int(req.GetPageSize())
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 100 {
		return nil, status.Error(codes.InvalidArgument, "некорректный page_size")
	}
	var after uuid.UUID
	if req.GetPageToken() != "" {
		raw, decodeErr := base64.RawURLEncoding.DecodeString(req.GetPageToken())
		if decodeErr == nil {
			after, decodeErr = uuid.FromBytes(raw)
		}
		if decodeErr != nil {
			return nil, status.Error(codes.InvalidArgument, "некорректный page_token")
		}
	}
	payments, err := s.repo.Payments(ctx, user.ID, after, limit+1)
	if err != nil {
		return nil, err
	}
	resp := &v1.ListMyPaymentsResponse{Payments: make([]*v1.Payment, 0, min(len(payments), limit))}
	if len(payments) > limit {
		payments = payments[:limit]
		last := payments[limit-1].ID
		resp.NextPageToken = base64.RawURLEncoding.EncodeToString(last[:])
	}
	for _, payment := range payments {
		resp.Payments = append(resp.Payments, paymentProto(payment))
	}
	return resp, nil
}

func planProto(plan Plan) *v1.Plan {
	return &v1.Plan{Code: plan.Code, Name: plan.Name, AmountKopecks: plan.AmountKopecks, PeriodDays: plan.PeriodDays, CompilationLimit: plan.CompilationLimit, CompilationWindowSeconds: plan.CompilationWindowSeconds, MonthlyTestRunLimit: plan.MonthlyTestRunLimit, HasMaterialAccess: plan.HasMaterialAccess, DailyTestGenerationLimit: plan.DailyTestGenerationLimit, DailyAiHintLimit: plan.DailyAIHintLimit}
}

func paymentProto(payment Payment) *v1.Payment {
	return &v1.Payment{Id: payment.ID.String(), PlanCode: payment.PlanCode, AmountKopecks: payment.AmountKopecks, Status: payment.Status, CreateTime: timestamppb.New(payment.CreatedAt)}
}
