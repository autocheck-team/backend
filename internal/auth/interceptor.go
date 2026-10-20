package auth

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TokenVerifier проверяет access-токен и возвращает пользователя.
type TokenVerifier interface {
	Verify(ctx context.Context, raw string) (Principal, error)
}

// OnAuthenticated вызывается после успешной проверки токена, например чтобы
// синхронизировать профиль пользователя в БД.
type OnAuthenticated func(ctx context.Context, p Principal) error

// Interceptors проверяет Bearer-токен у каждого gRPC-вызова, кроме публичных методов.
// grpc-gateway передаёт HTTP-заголовок Authorization в metadata как "authorization".
type Interceptors struct {
	verifier TokenVerifier
	onAuth   OnAuthenticated
	public   map[string]bool
}

// NewInterceptors создаёт интерсепторы. publicMethods — полные имена методов
// (например "/grpc.health.v1.Health/Check"), доступные без токена.
func NewInterceptors(v TokenVerifier, onAuth OnAuthenticated, publicMethods ...string) *Interceptors {
	public := make(map[string]bool, len(publicMethods))
	for _, m := range publicMethods {
		public[m] = true
	}
	return &Interceptors{verifier: v, onAuth: onAuth, public: public}
}

// Unary — интерсептор для unary-вызовов.
func (i *Interceptors) Unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		if i.public[info.FullMethod] {
			return h(ctx, req)
		}
		ctx, err := i.authenticate(ctx)
		if err != nil {
			return nil, err
		}
		return h(ctx, req)
	}
}

// Stream — интерсептор для потоковых вызовов.
func (i *Interceptors) Stream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
		if i.public[info.FullMethod] {
			return h(srv, ss)
		}
		ctx, err := i.authenticate(ss.Context())
		if err != nil {
			return err
		}
		return h(srv, &wrappedStream{ServerStream: ss, ctx: ctx})
	}
}

func (i *Interceptors) authenticate(ctx context.Context) (context.Context, error) {
	raw := bearerToken(ctx)
	if raw == "" {
		return nil, status.Error(codes.Unauthenticated, "требуется авторизация")
	}
	p, err := i.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "недействительный или просроченный токен")
	}
	if i.onAuth != nil {
		if err := i.onAuth(ctx, p); err != nil {
			return nil, err
		}
	}
	return WithPrincipal(ctx, p), nil
}

func bearerToken(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	for _, v := range md.Get("authorization") {
		if token, ok := strings.CutPrefix(v, "Bearer "); ok {
			return strings.TrimSpace(token)
		}
	}
	return ""
}

// Require возвращает пользователя из контекста и проверяет, что у него есть одна из ролей.
// Без ролей проверяет только, что запрос аутентифицирован.
func Require(ctx context.Context, roles ...Role) (Principal, error) {
	p, ok := FromContext(ctx)
	if !ok {
		return Principal{}, status.Error(codes.Unauthenticated, "требуется авторизация")
	}
	if len(roles) > 0 && !p.Has(roles...) {
		return Principal{}, status.Error(codes.PermissionDenied, "недостаточно прав")
	}
	return p, nil
}

type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context //nolint:containedctx // контекст потока с пользователем, иначе не передать
}

func (w *wrappedStream) Context() context.Context { return w.ctx }
