// Package server поднимает gRPC-сервер и grpc-gateway (REST) перед ним.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"buf.build/go/protovalidate"
	protovalidatemw "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/protovalidate"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/recovery"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"

	"github.com/autocheck-team/backend/internal/auth"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

// Config — адреса и параметры серверов.
type Config struct {
	GRPCAddr           string
	HTTPAddr           string
	CORSAllowedOrigins []string
	ShutdownTimeout    time.Duration
}

// Services — реализации gRPC-сервисов.
type Services struct {
	User    v1.UserServiceServer
	Problem v1.ProblemServiceServer
}

// Server — gRPC-сервер и HTTP-шлюз.
type Server struct {
	cfg  Config
	log  *slog.Logger
	db   Pinger
	grpc *grpc.Server
}

// New собирает сервер: интерсепторы, сервисы, health и reflection.
func New(
	cfg Config, log *slog.Logger, db Pinger, authn *auth.Interceptors,
	validator protovalidate.Validator, svc Services,
) *Server {
	recoveryOpt := recovery.WithRecoveryHandlerContext(func(ctx context.Context, p any) error {
		log.ErrorContext(ctx, "panic in handler", "panic", p)
		return status.Error(codes.Internal, "внутренняя ошибка сервера")
	})

	g := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			recovery.UnaryServerInterceptor(recoveryOpt),
			errorsUnary(log),
			authn.Unary(),
			protovalidatemw.UnaryServerInterceptor(validator),
		),
		grpc.ChainStreamInterceptor(
			recovery.StreamServerInterceptor(recoveryOpt),
			authn.Stream(),
		),
	)
	v1.RegisterUserServiceServer(g, svc.User)
	v1.RegisterProblemServiceServer(g, svc.Problem)

	hs := health.NewServer()
	healthpb.RegisterHealthServer(g, hs)
	reflection.Register(g)

	return &Server{cfg: cfg, log: log, db: db, grpc: g}
}

// PublicMethods — gRPC-методы, доступные без токена.
var PublicMethods = []string{
	healthpb.Health_Check_FullMethodName,
	healthpb.Health_Watch_FullMethodName,
	healthpb.Health_List_FullMethodName,
	"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo",
	"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo",
}

// Run обслуживает gRPC и HTTP до отмены ctx, затем плавно останавливает оба.
func (s *Server) Run(ctx context.Context) error {
	lis, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("listen grpc: %w", err)
	}

	// Шлюз ходит в собственный gRPC-сервер по сети, а не вызывает сервисы напрямую,
	// чтобы REST-запросы проходили через те же интерсепторы (авторизация, валидация).
	conn, err := grpc.NewClient(dialTarget(lis.Addr()), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial grpc: %w", err)
	}
	defer func() { _ = conn.Close() }()

	gw := runtime.NewServeMux()
	for _, register := range []func(context.Context, *runtime.ServeMux, *grpc.ClientConn) error{
		v1.RegisterUserServiceHandler,
		v1.RegisterProblemServiceHandler,
	} {
		if err := register(ctx, gw, conn); err != nil {
			return fmt.Errorf("register gateway: %w", err)
		}
	}

	httpSrv := &http.Server{
		Addr:              s.cfg.HTTPAddr,
		Handler:           cors(s.cfg.CORSAllowedOrigins, s.httpMux(gw)),
		ReadHeaderTimeout: 5 * time.Second,
	}

	eg, ctx := errgroup.WithContext(ctx)
	eg.Go(func() error {
		s.log.Info("grpc listening", "addr", lis.Addr().String())
		return s.grpc.Serve(lis)
	})
	eg.Go(func() error {
		s.log.Info("http listening", "addr", s.cfg.HTTPAddr)
		if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	eg.Go(func() error {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
		defer cancel()
		err := httpSrv.Shutdown(shutdownCtx)
		s.grpc.GracefulStop()
		return err
	})
	return eg.Wait()
}

// dialTarget превращает адрес слушателя ":9090" / "[::]:9090" в "localhost:9090".
func dialTarget(addr net.Addr) string {
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return "localhost:" + port
}

// errorsUnary не выпускает наружу внутренние ошибки: всё, что не gRPC-статус,
// логируется и превращается в Internal с безопасным текстом.
func errorsUnary(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		resp, err := h(ctx, req)
		if err == nil {
			return resp, nil
		}
		if _, ok := status.FromError(err); ok {
			return resp, err
		}
		log.ErrorContext(ctx, "rpc failed", "method", info.FullMethod, "err", err)
		return nil, status.Error(codes.Internal, "внутренняя ошибка сервера")
	}
}

// cors разрешает браузерные запросы с фронтенда (другой origin при локальной разработке).
func cors(allowed []string, next http.Handler) http.Handler {
	set := make(map[string]bool, len(allowed))
	for _, o := range allowed {
		set[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && set[origin] {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
