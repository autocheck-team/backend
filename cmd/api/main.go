// Команда api — gRPC-сервер и REST-шлюз (grpc-gateway) для фронтенда.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/autocheck-team/backend/internal/auth"
	"github.com/autocheck-team/backend/internal/config"
	"github.com/autocheck-team/backend/internal/db"
	"github.com/autocheck-team/backend/internal/server"
	"github.com/autocheck-team/backend/internal/user"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
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

	srv, err := server.New(server.Config{
		GRPCAddr:           cfg.GRPCAddr,
		HTTPAddr:           cfg.HTTPAddr,
		CORSAllowedOrigins: cfg.CORSAllowedOrigins,
		ShutdownTimeout:    cfg.ShutdownTimeout,
	}, log, pool, authn, server.Services{
		User: user.NewService(),
	})
	if err != nil {
		return err
	}
	return srv.Run(ctx)
}
