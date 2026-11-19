package monitoring

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/google/uuid"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

// Unary измеряет также отказы авторизации, валидации и восстановленные panic.
func (m *Metrics) Unary(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		state := newRequestLog(log)
		state.submission(info.FullMethod, req, nil)
		ctx = context.WithValue(ctx, logKey{}, state)
		started := time.Now()
		response, err := next(ctx, req)
		m.Request("grpc", info.FullMethod, status.Code(err).String(), time.Since(started))
		if uuid.Validate(submissionID(info.FullMethod, req, nil)) != nil {
			state.submission(info.FullMethod, req, response)
		}
		Logger(ctx, log).InfoContext(ctx, "rpc completed", "method", info.FullMethod, "code", status.Code(err).String())
		return response, err
	}
}

func submissionID(method string, request, response any) string {
	switch message := response.(type) {
	case *v1.CreateSubmissionResponse:
		return message.GetSubmission().GetId()
	case *v1.GetSubmissionResponse:
		return message.GetSubmission().GetId()
	}
	if strings.HasPrefix(method, "/autocheck.v1.SubmissionService/") {
		if message, ok := request.(proto.Message); ok {
			reflected := message.ProtoReflect()
			if field := reflected.Descriptor().Fields().ByName("id"); field != nil {
				return reflected.Get(field).String()
			}
		}
	}
	return ""
}

// Stream измеряет весь срок подписки до закрытия потока.
func (m *Metrics) Stream(log *slog.Logger) grpc.StreamServerInterceptor {
	return func(service any, stream grpc.ServerStream, info *grpc.StreamServerInfo, next grpc.StreamHandler) error {
		state := newRequestLog(log)
		wrapped := &loggedStream{ServerStream: stream, state: state, method: info.FullMethod, ctx: context.WithValue(stream.Context(), logKey{}, state)}
		started := time.Now()
		err := next(service, wrapped)
		m.Request("grpc", info.FullMethod, status.Code(err).String(), time.Since(started))
		Logger(wrapped.Context(), log).InfoContext(wrapped.Context(), "stream completed", "method", info.FullMethod, "code", status.Code(err).String())
		return err
	}
}

type (
	routeKey struct{}
	route    struct{ pattern string }
)

// GatewayRoute получает шаблон уже выбранного gateway-маршрута до разбора запроса.
func (m *Metrics) GatewayRoute(next runtime.HandlerFunc) runtime.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, params map[string]string) {
		if state, ok := r.Context().Value(routeKey{}).(*route); ok {
			if pattern, found := runtime.HTTPPattern(r.Context()); found {
				state.pattern = strings.ReplaceAll(pattern.String(), "=*}", "}")
			}
		}
		next(w, r, params)
	}
}

// Gateway сохраняет Flush для watch-потоков и объединяет неизвестные пути в unmatched.
func (m *Metrics) Gateway(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := &route{pattern: "unmatched"}
		measured := httpsnoop.CaptureMetrics(next, w, r.WithContext(context.WithValue(r.Context(), routeKey{}, state)))
		method := r.Method
		switch method {
		case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		default:
			method = "OTHER"
		}
		m.Request("http", method+" "+state.pattern, strconv.Itoa(measured.Code), measured.Duration)
	})
}

type logKey struct{}

type requestLog struct{ logger atomic.Pointer[slog.Logger] }

func newRequestLog(log *slog.Logger) *requestLog {
	state := &requestLog{}
	state.logger.Store(log)
	return state
}

func (s *requestLog) submission(method string, request, response any) {
	if id := submissionID(method, request, response); uuid.Validate(id) == nil {
		s.logger.Store(s.logger.Load().With("submission_id", id))
	}
}

// Logger возвращает логгер запроса с ID посылки, если сообщение уже прочитано.
func Logger(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if state, ok := ctx.Value(logKey{}).(*requestLog); ok {
		return state.logger.Load()
	}
	return fallback
}

type loggedStream struct {
	grpc.ServerStream
	ctx    context.Context
	state  *requestLog
	method string
}

func (s *loggedStream) Context() context.Context { return s.ctx }

func (s *loggedStream) RecvMsg(message any) error {
	if err := s.ServerStream.RecvMsg(message); err != nil {
		return err
	}
	s.state.submission(s.method, message, nil)
	return nil
}
