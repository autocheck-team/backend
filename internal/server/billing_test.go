package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/autocheck-team/backend/internal/auth"
	"github.com/autocheck-team/backend/internal/billing"
	"github.com/autocheck-team/backend/internal/db"
	"github.com/autocheck-team/backend/internal/submission"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

type billingVerifier struct{ user auth.Principal }

func (v billingVerifier) Verify(context.Context, string) (auth.Principal, error) { return v.user, nil }

type billingObjects struct{}

func (billingObjects) Put(context.Context, string, io.Reader, int64, string) error { return nil }
func (billingObjects) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(nil)), nil
}

// TestBillingGateway прогоняет HTTP → gRPC → авторизацию/валидацию → БД → HTTP провайдера.
func TestBillingGateway(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	admin, err := db.Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := "gateway_" + uuid.New().String()[:8]
	if _, err := admin.Exec(t.Context(), `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
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
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	userID, problemID := uuid.New(), uuid.New()
	for _, query := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id,email,display_name) VALUES ($1,$2,'gateway')`, []any{userID, userID.String() + "@test.local"}},
		{`INSERT INTO problems (id,author_id,slug,title,statement,time_limit_ms,memory_limit_kb,is_published) VALUES ($1,$2,'gateway','Gateway','test',1000,65536,true)`, []any{problemID, userID}},
		{`INSERT INTO problem_languages (problem_id,language_id) VALUES ($1,'go1.27')`, []any{problemID}},
		{`INSERT INTO test_cases (problem_id,ordinal,input_key,expected_key,is_ready) VALUES ($1,1,'in','expected',true)`, []any{problemID}},
	} {
		if _, err := pool.Exec(t.Context(), query.sql, query.args...); err != nil {
			t.Fatal(err)
		}
	}
	var paid atomic.Bool
	var creates atomic.Int32
	var paymentID atomic.Value
	paymentID.Store("")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var body struct {
				Metadata map[string]string `json:"metadata"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			// HTTP-обработчики живут в разных горутинах, даже последовательные запросы требуют синхронизации.
			paymentID.Store(body.Metadata["payment_id"])
			creates.Add(1)
		}
		state := "pending"
		if paid.Load() {
			state = "succeeded"
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"id": "provider-gateway", "status": state, "paid": paid.Load(), "amount": map[string]string{"value": "299.00", "currency": "RUB"}, "metadata": map[string]string{"payment_id": paymentID.Load().(string)}, "confirmation": map[string]string{"confirmation_url": "https://yookassa.ru/confirm/test"}}); err != nil {
			t.Error(err)
		}
	}))
	defer provider.Close()
	paymentProvider, err := billing.NewYookassa("shop", "secret", provider.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := billing.NewService(billing.NewRepository(pool), paymentProvider, logger)
	validator, err := protovalidate.New()
	if err != nil {
		t.Fatal(err)
	}
	authn := auth.NewInterceptors(billingVerifier{user: auth.Principal{ID: userID, Roles: []auth.Role{auth.RoleStudent}}}, nil, PublicMethods...)
	address := func() string {
		t.Helper()
		listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := listener.Addr().String()
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		return addr
	}
	grpcAddr, httpAddr := address(), address()
	server := New(Config{GRPCAddr: grpcAddr, HTTPAddr: httpAddr, ShutdownTimeout: time.Second}, logger, pool, authn, validator, Services{
		User: &v1.UnimplementedUserServiceServer{}, Problem: &v1.UnimplementedProblemServiceServer{}, Billing: service,
		Submission: submission.NewService(submission.NewRepository(pool), billingObjects{}),
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("server failed to stop")
		}
	})
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	request := func(method, path, body string, authorized bool) (int, map[string]any) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, "http://"+httpAddr+path, bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if authorized {
			req.Header.Set("Authorization", "Bearer test")
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := response.Body.Close(); err != nil {
				t.Error(err)
			}
		}()
		var result map[string]any
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, result
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+httpAddr+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(req)
		if err == nil {
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code, _ := request(http.MethodGet, "/v1/plans", "", false); code != 200 {
		t.Fatalf("public plans=%d", code)
	}
	if code, _ := request(http.MethodGet, "/v1/subscriptions/me", "", false); code != 401 {
		t.Fatalf("anonymous subscription=%d", code)
	}
	if code, _ := request(http.MethodPost, "/v1/subscriptions:checkout", `{"planCode":"pro","returnUrl":"https://example.com","requestId":"invalid"}`, true); code != 400 {
		t.Fatalf("validation=%d", code)
	}
	submissionBody := fmt.Sprintf(`{"problemId":%q,"languageId":"go1.27","source":"package main"}`, problemID.String())
	if code, _ := request(http.MethodPost, "/v1/problems/"+problemID.String()+"/submissions", submissionBody, true); code != 200 {
		t.Fatalf("first submission=%d", code)
	}
	code, limited := request(http.MethodPost, "/v1/problems/"+problemID.String()+"/submissions", submissionBody, true)
	if code != 429 || limited["details"] == nil {
		t.Fatalf("compilation quota: code=%d response=%v", code, limited)
	}
	checkout := fmt.Sprintf(`{"planCode":"pro","returnUrl":"https://example.com","requestId":%q}`, uuid.NewString())
	code, response := request(http.MethodPost, "/v1/subscriptions:checkout", checkout, true)
	if code != 200 || response["confirmationUrl"] == nil {
		t.Fatalf("checkout=%d", code)
	}
	if code, _ := request(http.MethodPost, "/v1/subscriptions:checkout", checkout, true); code != 200 || creates.Load() != 1 {
		t.Fatal("checkout retry created another payment")
	}
	// Тело похоже на настоящее уведомление: незнакомые поля объекта должны игнорироваться.
	webhook := `{"type":"notification","event":"payment.succeeded","object":{"id":"provider-gateway","status":"succeeded","amount":{"value":"0.01","currency":"USD"}}}`
	if code, _ := request(http.MethodPost, "/v1/billing/yookassa/webhook", webhook, false); code != 200 {
		t.Fatalf("public webhook=%d", code)
	}
	_, response = request(http.MethodGet, "/v1/subscriptions/me", "", true)
	if response["plan"].(map[string]any)["code"] != "free" {
		t.Fatal("webhook trusted forged status")
	}
	paid.Store(true)
	for range 2 {
		if code, _ := request(http.MethodPost, "/v1/billing/yookassa/webhook", webhook, false); code != 200 {
			t.Fatalf("paid webhook=%d", code)
		}
	}
	_, response = request(http.MethodGet, "/v1/subscriptions/me", "", true)
	if response["plan"].(map[string]any)["code"] != "pro" {
		t.Fatal("verified payment did not activate pro")
	}
	if code, _ := request(http.MethodPost, "/v1/problems/"+problemID.String()+"/submissions", submissionBody, true); code != 200 {
		t.Fatalf("pro compilation=%d", code)
	}
	if code, response := request(http.MethodGet, "/v1/payments/me?pageSize=1", "", true); code != 200 || len(response["payments"].([]any)) != 1 {
		t.Fatalf("payments=%d", code)
	}
	t.Log("gateway: public routes, JWT requirement, validation, 429 with RetryInfo, checkout retry, verified webhook and pro compilation passed")
}
