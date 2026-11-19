package recommendation

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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/rpc/status"
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

func TestGatewayRecommendation(t *testing.T) {
	// arrange
	f := setup(t)
	f.svc.cfg.DailyLimit = 2
	g := startGateway(t, f)
	request := &v1.RecommendProblemsRequest{Goal: "графы", LanguageId: "go1.27"}
	const path = "/v1/problems:recommend"
	// act
	g.call(t, auth.Principal{}, http.MethodPost, path, request, nil, http.StatusUnauthorized)
	g.call(t, f.student, http.MethodPost, path, &v1.RecommendProblemsRequest{Goal: " "}, nil, http.StatusBadRequest)
	for _, principal := range []auth.Principal{f.student, f.author, f.admin} {
		response := &v1.RecommendProblemsResponse{}
		g.call(t, principal, http.MethodPost, path, request, response, http.StatusOK)
		// assert
		if !response.IsTextFallback || len(response.Recommendations) != 1 || response.Recommendations[0].ProblemId != f.graph.String() {
			t.Fatalf("gateway response=%v", response)
		}
	}
	g.call(t, f.student, http.MethodPost, path, request, nil, http.StatusOK)
	exhausted := &status.Status{}
	g.call(t, f.student, http.MethodPost, path, request, exhausted, http.StatusTooManyRequests)
	if exhausted.Code != 8 || len(exhausted.Details) != 1 || !strings.HasSuffix(exhausted.Details[0].TypeUrl, "google.rpc.RetryInfo") {
		t.Fatalf("quota status=%v", exhausted)
	}
}

func TestGatewaySemanticRefresh(t *testing.T) {
	// arrange
	f := setup(t)
	var embeddings, chats atomic.Int32
	embed := embeddingHandler(t, &embeddings)
	f.svc.model = provider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/embeddings" {
			embed(w, r)
			return
		}
		chats.Add(1)
		encode(t, w, map[string]any{"choices": []any{map[string]any{"message": map[string]string{
			"content": `{"explanation":"Задание поможет освоить кратчайшие пути в графах."}`,
		}}}})
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	failures := make(chan error, 10)
	go func() { defer close(done); f.svc.Run(ctx, func(err error) { failures <- err }) }()
	t.Cleanup(func() {
		cancel()
		<-done
		close(failures)
		for err := range failures {
			t.Error(err)
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		var count int
		if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM problem_embeddings WHERE embedding IS NOT NULL`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background index not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	g := startGateway(t, f)
	response := &v1.RecommendProblemsResponse{}
	// act
	g.call(t, f.student, http.MethodPost, "/v1/problems:recommend", &v1.RecommendProblemsRequest{
		Goal: "Хочу изучать графы", LanguageId: "go1.27", Difficulty: "начальная", Limit: 10,
	}, response, http.StatusOK)
	// assert
	if response.IsTextFallback || response.Explanation == "" || len(response.Recommendations) != 1 ||
		response.Recommendations[0].ProblemId != f.graph.String() || response.Recommendations[0].Score != 1 ||
		embeddings.Load() != 2 || chats.Load() != 1 {
		t.Fatalf("semantic gateway response=%v embeddings=%d chats=%d", response, embeddings.Load(), chats.Load())
	}
	t.Log("POST /v1/problems:recommend: HTTP 200, published graph problem, cosine=1, one explanation; background batch persisted")
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
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "generation-test"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &key.PublicKey, KeyID: "generation-test", Algorithm: string(jose.RS256), Use: "sig",
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
	const issuer = "http://generation.test/realms/autocheck"
	gateway := &gateway{client: &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}, tokens: make(map[uuid.UUID]string)}
	for _, principal := range []auth.Principal{f.student, f.author, f.admin} {
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
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), f.pool, authn, f.validator, server.Services{
		User: &v1.UnimplementedUserServiceServer{}, Problem: &v1.UnimplementedProblemServiceServer{},
		Submission: &v1.UnimplementedSubmissionServiceServer{}, Feedback: &v1.UnimplementedFeedbackServiceServer{}, Recommendation: f.svc,
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
