package feedback

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/autocheck-team/backend/internal/auth"
	"github.com/autocheck-team/backend/internal/server"
	"github.com/autocheck-team/backend/internal/user"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

type gateway struct {
	base   string
	client *http.Client
	tokens map[uuid.UUID]string
}

// startGateway использует настоящие JWT-проверку, gRPC-интерсепторы и сетевой шлюз.
// Только JWKS подменён локальным сервером, чтобы тест не зависел от общего Keycloak.
func startGateway(t *testing.T, f *fixture) *gateway {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "feedback-test"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &key.PublicKey, KeyID: "feedback-test", Algorithm: string(jose.RS256), Use: "sig",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write(keys); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(jwks.Close)
	const issuer = "http://feedback.test/realms/autocheck"
	gateway := &gateway{client: &http.Client{
		Timeout:   3 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}, tokens: make(map[uuid.UUID]string)}
	for _, principal := range []auth.Principal{f.student, f.other, f.author, f.admin} {
		token, err := jwt.Signed(signer).Claims(map[string]any{
			"iss": issuer, "aud": "autocheck-api", "sub": principal.ID.String(),
			"exp": time.Now().Add(5 * time.Minute).Unix(), "email": principal.Email, "name": "Feedback",
			"realm_access": map[string]any{"roles": principal.Roles},
		}).Serialize()
		if err != nil {
			t.Fatal(err)
		}
		gateway.tokens[principal.ID] = token
	}
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	gateway.base = "http://" + addr
	ctx, cancel := context.WithCancel(t.Context())
	verifier := auth.NewVerifier(ctx, issuer, jwks.URL, "autocheck-api")
	authn := auth.NewInterceptors(verifier, user.NewSyncer(f.pool).Sync, server.PublicMethods...)
	srv := server.New(server.Config{
		GRPCAddr: "127.0.0.1:0", HTTPAddr: addr, ShutdownTimeout: 3 * time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), f.pool, authn, testValidator(t), server.Services{
		User: &v1.UnimplementedUserServiceServer{}, Problem: &v1.UnimplementedProblemServiceServer{},
		Submission: &v1.UnimplementedSubmissionServiceServer{}, Feedback: f.svc,
	})
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("gateway shutdown timed out")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, gateway.base+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := gateway.client.Do(req)
		if err == nil {
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return gateway
}

func (g *gateway) call(t *testing.T, principal auth.Principal, method, path string,
	request, response proto.Message, want int,
) {
	t.Helper()
	var data []byte
	var err error
	if request != nil {
		data, err = protojson.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequestWithContext(t.Context(), method, g.base+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if token := g.tokens[principal.ID]; token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, resp.StatusCode, want, body)
	}
	if response != nil {
		if err := protojson.Unmarshal(body, response); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGatewayFeedbackLifecycle(t *testing.T) {
	f := setup(t)
	g := startGateway(t, f)
	const path = "/v1/feedback-requests"
	created := &v1.CreateFeedbackRequestResponse{}
	req := validCreate()
	req.ClientRequestId, req.LanguageId = uuid.NewString(), proto.String("go1.27")
	g.call(t, f.student, http.MethodPost, path, req, created, http.StatusOK)
	id := created.GetFeedbackRequest().GetId()
	if id == "" || created.GetFeedbackRequest().GetUserId() != f.student.ID.String() ||
		created.GetFeedbackRequest().GetStatus() != v1.FeedbackRequestStatus_FEEDBACK_REQUEST_STATUS_NEW ||
		created.GetFeedbackRequest().GetLanguageId() != "go1.27" {
		t.Fatalf("created=%v", created)
	}
	get := &v1.GetFeedbackRequestResponse{}
	for _, principal := range []auth.Principal{f.student, f.admin} {
		g.call(t, principal, http.MethodGet, path+"/"+id, nil, get, http.StatusOK)
	}
	for _, principal := range []auth.Principal{f.other, f.author} {
		g.call(t, principal, http.MethodGet, path+"/"+id, nil, nil, http.StatusNotFound)
	}
	g.call(t, auth.Principal{}, http.MethodPost, path, req, nil, http.StatusUnauthorized)
	g.call(t, auth.Principal{}, http.MethodGet, path, nil, nil, http.StatusUnauthorized)
	g.call(t, f.student, http.MethodGet, path+"/"+uuid.NewString(), nil, nil, http.StatusNotFound)
	invalid := validCreate()
	invalid.LanguageId = proto.String("missing")
	g.call(t, f.student, http.MethodPost, path, invalid, nil, http.StatusBadRequest)
	invalid = validCreate()
	invalid.Title = ""
	g.call(t, f.student, http.MethodPost, path, invalid, nil, http.StatusBadRequest)
	g.call(t, f.student, http.MethodGet, path+"?page_size=101", nil, nil, http.StatusBadRequest)
	g.call(t, f.student, http.MethodGet, path+"?type=99", nil, nil, http.StatusBadRequest)
	g.call(t, f.student, http.MethodGet, path+"?page_token=invalid", nil, nil, http.StatusBadRequest)
	// Один ключ сохраняет первый результат даже при изменении полезной нагрузки.
	req.Title = "Повтор"
	repeated := &v1.CreateFeedbackRequestResponse{}
	g.call(t, f.student, http.MethodPost, path, req, repeated, http.StatusOK)
	if !proto.Equal(created, repeated) {
		t.Fatal("idempotency changed the response")
	}
	for _, principal := range []auth.Principal{f.author, f.admin, f.other} {
		g.call(t, principal, http.MethodPost, path, validCreate(), &v1.CreateFeedbackRequestResponse{}, http.StatusOK)
	}
	for _, principal := range []auth.Principal{f.student, f.author, f.other} {
		list := &v1.ListFeedbackRequestsResponse{}
		g.call(t, principal, http.MethodGet, path, nil, list, http.StatusOK)
		if len(list.GetFeedbackRequests()) != 1 || list.GetFeedbackRequests()[0].GetUserId() != principal.ID.String() {
			t.Fatalf("own list=%v", list)
		}
	}
	// Между страницами вставляем новый запрос: keyset не возвращает уже прочитанные строки.
	list := &v1.ListFeedbackRequestsResponse{}
	g.call(t, f.admin, http.MethodGet, path+"?page_size=2", nil, list, http.StatusOK)
	if len(list.GetFeedbackRequests()) != 2 || list.GetNextPageToken() == "" {
		t.Fatalf("page=%v", list)
	}
	seen := map[string]bool{}
	for _, request := range list.GetFeedbackRequests() {
		seen[request.GetId()] = true
	}
	token := list.GetNextPageToken()
	g.call(t, f.admin, http.MethodPost, path, validCreate(), &v1.CreateFeedbackRequestResponse{}, http.StatusOK)
	g.call(t, f.admin, http.MethodGet, path+"?page_size=2&page_token="+url.QueryEscape(token), nil, list, http.StatusOK)
	if len(list.GetFeedbackRequests()) != 2 || list.GetNextPageToken() != "" {
		t.Fatalf("last page=%v", list)
	}
	for _, request := range list.GetFeedbackRequests() {
		if seen[request.GetId()] {
			t.Fatal("duplicate across pages")
		}
	}
	update := &v1.UpdateFeedbackRequestRequest{
		Status:       v1.FeedbackRequestStatus_FEEDBACK_REQUEST_STATUS_ACCEPTED,
		AdminComment: "Запланировано",
	}
	for _, principal := range []auth.Principal{f.student, f.author} {
		g.call(t, principal, http.MethodPatch, path+"/"+id, update, nil, http.StatusForbidden)
	}
	updated := &v1.UpdateFeedbackRequestResponse{}
	g.call(t, f.admin, http.MethodPatch, path+"/"+id, update, updated, http.StatusOK)
	if updated.GetFeedbackRequest().GetAdminComment() != update.GetAdminComment() {
		t.Fatal("missing admin comment")
	}
	g.call(t, f.admin, http.MethodGet, path+"?status=FEEDBACK_REQUEST_STATUS_ACCEPTED&type=FEEDBACK_REQUEST_TYPE_COMPILER_UPDATE", nil, list, http.StatusOK)
	if len(list.GetFeedbackRequests()) != 1 || list.GetFeedbackRequests()[0].GetId() != id {
		t.Fatalf("filtered=%v", list)
	}
	update.Status = v1.FeedbackRequestStatus_FEEDBACK_REQUEST_STATUS_COMPLETED
	g.call(t, f.admin, http.MethodPatch, path+"/"+id, update, updated, http.StatusOK)
	update.AdminComment = ""
	g.call(t, f.admin, http.MethodPatch, path+"/"+id, update, updated, http.StatusOK)
	if updated.GetFeedbackRequest().GetAdminComment() != "" {
		t.Fatal("comment not cleared")
	}
	update.Status = v1.FeedbackRequestStatus_FEEDBACK_REQUEST_STATUS_NEW
	g.call(t, f.admin, http.MethodPatch, path+"/"+id, update, nil, http.StatusBadRequest)
	update.Status = v1.FeedbackRequestStatus_FEEDBACK_REQUEST_STATUS_ACCEPTED
	g.call(t, f.admin, http.MethodPatch, path+"/"+uuid.NewString(), update, nil, http.StatusNotFound)
	update.AdminComment = strings.Repeat("x", 5001)
	g.call(t, f.admin, http.MethodPatch, path+"/"+id, update, nil, http.StatusBadRequest)
	t.Log("REST: create/replay, own/admin lists, pagination, filters, get permissions, admin transitions and validation passed")
}
