// Команда api — gRPC-сервер и REST-шлюз (grpc-gateway) для фронтенда.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"buf.build/go/protovalidate"
	"golang.org/x/sync/errgroup"

	"github.com/autocheck-team/backend/internal/auth"
	"github.com/autocheck-team/backend/internal/billing"
	"github.com/autocheck-team/backend/internal/config"
	"github.com/autocheck-team/backend/internal/db"
	"github.com/autocheck-team/backend/internal/feedback"
	"github.com/autocheck-team/backend/internal/problem"
	"github.com/autocheck-team/backend/internal/server"
	"github.com/autocheck-team/backend/internal/storage"
	"github.com/autocheck-team/backend/internal/submission"
	"github.com/autocheck-team/backend/internal/user"

	"github.com/autocheck-team/backend/internal/monitoring"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "api")
	if err := run(log); err != nil {
		log.Error("api stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.RequireAPI(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Ключи Keycloak подгружаются в фоне на всё время жизни процесса.
	verifier := auth.NewVerifier(context.Background(), cfg.OIDCIssuer, cfg.OIDCJWKSURL, cfg.OIDCAudience)
	syncer := user.NewSyncer(pool)
	authn := auth.NewInterceptors(verifier, syncer.Sync, server.PublicMethods...)

	objects, err := storage.New(ctx, storage.Config{
		Endpoint:       cfg.S3Endpoint,
		PublicEndpoint: cfg.S3PublicEndpoint,
		Bucket:         cfg.S3Bucket,
		AccessKey:      cfg.S3AccessKey,
		SecretKey:      cfg.S3SecretKey,
	})
	if err != nil {
		return err
	}
	validator, err := protovalidate.New()
	if err != nil {
		return err
	}
	events := submission.NewEvents(pool.Config().ConnConfig, log)
	listenCtx, cancelListener := context.WithCancel(ctx)
	var listeners sync.WaitGroup
	listeners.Go(func() { events.Run(listenCtx) })
	defer func() {
		cancelListener()
		listeners.Wait()
	}()

	provider, err := billing.NewYookassa(cfg.YookassaShopID, cfg.YookassaSecretKey, cfg.YookassaAPIURL, cfg.YookassaTimeout)
	if err != nil {
		return err
	}
	// Не передаём типизированный nil как интерфейс: иначе выключенная платёжка выглядит включённой.
	var paymentProvider billing.Provider
	if provider != nil {
		paymentProvider = provider
	}
	metrics := monitoring.New("api")
	billingRepository := billing.NewRepository(pool)
	billingRepository.Metrics = metrics
	billingService := billing.NewService(billingRepository, paymentProvider, log)
	submissionService := submission.NewService(submission.NewRepository(pool), objects, events)
	submissionService.Metrics = metrics
	srv := server.New(server.Config{
		GRPCAddr:           cfg.GRPCAddr,
		Metrics:            metrics,
		HTTPAddr:           cfg.HTTPAddr,
		CORSAllowedOrigins: cfg.CORSAllowedOrigins,
		ShutdownTimeout:    cfg.ShutdownTimeout,
	}, log, pool, authn, validator, server.Services{
		User:       user.NewService(),
		Billing:    billingService,
		Problem:    problem.NewService(problem.NewRepository(pool), objects, validator),
		Submission: submissionService,
		Feedback:   feedback.NewService(feedback.NewRepository(pool, cfg.FeedbackQueryTimeout), validator),
	})
	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error { return srv.Run(ctx) })
	group.Go(func() error { return metrics.Run(ctx, cfg.MetricsAddr, cfg.ShutdownTimeout) })
	group.Go(func() error { return billingService.RunSync(ctx, cfg.BillingSyncInterval) })
	return group.Wait()
}
