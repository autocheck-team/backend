package monitoring

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestGatewayRouteCardinalityAndFlush(t *testing.T) {
	t.Parallel()
	// arrange
	metrics := New("api")
	mux := runtime.NewServeMux(runtime.WithMiddlewares(metrics.GatewayRoute))
	if err := mux.HandlePath("GET", "/v1/submissions/{id}:watch", func(w http.ResponseWriter, _ *http.Request, _ map[string]string) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("streaming flush lost")
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		flusher.Flush()
	}); err != nil {
		t.Fatal(err)
	}
	// act
	for _, path := range []string{"/v1/submissions/one:watch", "/v1/submissions/two:watch", "/unknown/one", "/unknown/two"} {
		metrics.Gateway(mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), "GET", path, nil))
	}
	// assert
	if got := testutil.ToFloat64(metrics.requests.WithLabelValues("http", "GET /v1/submissions/{id}:watch", "401")); got != 2 {
		t.Fatalf("matched requests=%v", got)
	}
	if got := testutil.ToFloat64(metrics.requests.WithLabelValues("http", "GET unmatched", "404")); got != 2 {
		t.Fatalf("unmatched requests=%v", got)
	}
	response := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), "GET", "/metrics", nil))
	if strings.Contains(response.Body.String(), "/unknown/one") || strings.Contains(response.Body.String(), "/submissions/one") {
		t.Fatal("raw path leaked into labels")
	}
}

func TestUnaryStatusAndRegistryIsolation(t *testing.T) {
	t.Parallel()
	// arrange
	first, second := New("api"), New("api")
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	// act
	_, err := first.Unary(log)(t.Context(), nil, &grpc.UnaryServerInfo{FullMethod: "/test.Service/Call"}, func(context.Context, any) (any, error) { return nil, status.Error(codes.Unauthenticated, "denied") })
	// assert
	if status.Code(err) != codes.Unauthenticated {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(first.requests.WithLabelValues("grpc", "/test.Service/Call", "Unauthenticated")); got != 1 {
		t.Fatal(got)
	}
	if got := testutil.ToFloat64(second.requests.WithLabelValues("grpc", "/test.Service/Call", "Unauthenticated")); got != 0 {
		t.Fatal(got)
	}
}

func TestAttemptAndQueueFailure(t *testing.T) {
	t.Parallel()
	// arrange
	metrics := New("worker")
	// act
	done := metrics.BeginAttempt()
	if got := testutil.ToFloat64(metrics.active); got != 1 {
		t.Fatal(got)
	}
	done()
	metrics.Queue(2, 1, nil)
	metrics.Queue(0, 0, errors.New("database unavailable"))
	// assert
	if got := testutil.ToFloat64(metrics.active); got != 0 {
		t.Fatal(got)
	}
	if got := testutil.ToFloat64(metrics.queue.WithLabelValues("waiting")); got != 2 {
		t.Fatal(got)
	}
	if got := testutil.ToFloat64(metrics.queuePoll); got != 0 {
		t.Fatal(got)
	}
	if got := testutil.ToFloat64(metrics.queueUpdated); got <= 0 {
		t.Fatal(got)
	}
}

func TestMetricsListenerLifecycle(t *testing.T) {
	t.Parallel()
	// arrange
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- New("worker").Run(ctx, address, time.Second) }()
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	// act
	deadline := time.Now().Add(5 * time.Second)
	for {
		request, err := http.NewRequestWithContext(t.Context(), "GET", "http://"+address+"/metrics", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if readErr != nil || closeErr != nil {
				t.Fatal(errors.Join(readErr, closeErr))
			}
			if response.StatusCode != 200 || !strings.Contains(string(body), "autocheck_judge_active{service=\"worker\"} 0") {
				t.Fatalf("metrics response status=%d", response.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	// assert
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("metrics listener did not stop")
	}
}

type metricStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s metricStream) Context() context.Context { return s.ctx }

func (metricStream) RecvMsg(message any) error {
	proto.Merge(message.(proto.Message), &v1.WatchSubmissionRequest{Id: "019a9de2-0000-7000-8000-000000000001"})
	return nil
}

func TestStreamMetricsAndSubmissionLogs(t *testing.T) {
	t.Parallel()
	// arrange
	var output bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&output, nil)).With("service", "api")
	metrics := New("api")
	method := v1.SubmissionService_WatchSubmission_FullMethodName
	// act
	err := metrics.Stream(log)(nil, metricStream{ctx: t.Context()}, &grpc.StreamServerInfo{FullMethod: method, IsServerStream: true}, func(_ any, stream grpc.ServerStream) error {
		if err := stream.RecvMsg(&v1.WatchSubmissionRequest{}); err != nil {
			return err
		}
		Logger(stream.Context(), log).ErrorContext(stream.Context(), "stream failed")
		return status.Error(codes.Aborted, "lease changed")
	})
	// assert
	if status.Code(err) != codes.Aborted {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(metrics.requests.WithLabelValues("grpc", method, "Aborted")); got != 1 {
		t.Fatal(got)
	}
	decoder := json.NewDecoder(&output)
	for range 2 {
		var entry map[string]any
		if err := decoder.Decode(&entry); err != nil {
			t.Fatal(err)
		}
		if entry["service"] != "api" || entry["submission_id"] != "019a9de2-0000-7000-8000-000000000001" {
			t.Fatal("missing log correlation")
		}
	}
}
