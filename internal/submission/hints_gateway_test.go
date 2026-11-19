package submission

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"buf.build/go/protovalidate"
	"github.com/autocheck-team/backend/internal/auth"
	"github.com/autocheck-team/backend/internal/llm"
	"github.com/autocheck-team/backend/internal/problem"
	"github.com/autocheck-team/backend/internal/server"
	"github.com/autocheck-team/backend/internal/user"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
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
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "hint-test"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &key.PublicKey, KeyID: "hint-test", Algorithm: string(jose.RS256), Use: "sig",
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
	const issuer = "http://hint.test/realms/autocheck"
	gateway := &gateway{client: &http.Client{
		Timeout:   3 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}, tokens: make(map[uuid.UUID]string)}
	for _, principal := range []auth.Principal{f.student, f.student2, f.author} {
		token, err := jwt.Signed(signer).Claims(map[string]any{
			"iss": issuer, "aud": "autocheck-api", "sub": principal.ID.String(),
			"exp": time.Now().Add(5 * time.Minute).Unix(), "email": principal.Email, "name": "Hint",
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
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), f.pool, authn, hintValidator(t), server.Services{
		User: &v1.UnimplementedUserServiceServer{}, Problem: problem.NewService(problem.NewRepository(f.pool), nil, hintValidator(t)),
		Submission: f.svc, Feedback: &v1.UnimplementedFeedbackServiceServer{},
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

func hintValidator(t *testing.T) protovalidate.Validator {
	t.Helper()
	validator, err := protovalidate.New()
	if err != nil {
		t.Fatal(err)
	}
	return validator
}

func TestHintGatewayLifecycle(t *testing.T) {
	// arrange
	f, _ := hintSetup(t, nil)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request struct {
			Messages []llm.Message `json:"messages"`
			Provider struct {
				ZDR bool `json:"zdr"`
			} `json:"provider"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Provider.ZDR || len(request.Messages) != 3 {
			t.Error("incorrect LLM request")
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": `{"hint":"Как изменится ответ, если все числа отрицательные?"}`}}}}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(provider.Close)
	model, err := llm.New(llm.Config{BaseURL: provider.URL, APIKey: "stub", Retries: 0})
	if err != nil {
		t.Fatal(err)
	}
	f.svc.hints.model = model
	req := hintReady(t, f, "WA")
	g := startGateway(t, f)
	path := "/v1/submissions/" + req.Id
	update := &v1.UpdateProblemRequest{Problem: &v1.Problem{Id: f.published.String(), AllowAiHints: true}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"allow_ai_hints"}}}
	// act / assert
	g.call(t, auth.Principal{}, http.MethodPost, path+":hint", req, nil, http.StatusUnauthorized)
	g.call(t, f.student2, http.MethodPost, path+":hint", req, nil, http.StatusNotFound)
	g.call(t, f.author, http.MethodPost, path+":hint", req, nil, http.StatusNotFound)
	g.call(t, f.student, http.MethodPost, path+":hint", req, nil, http.StatusBadRequest)
	g.call(t, f.student, http.MethodPatch, "/v1/problems/"+f.published.String()+"?update_mask=allow_ai_hints", update.Problem, nil, http.StatusForbidden)
	updated := &v1.UpdateProblemResponse{}
	g.call(t, f.author, http.MethodPatch, "/v1/problems/"+f.published.String()+"?update_mask=allow_ai_hints", update.Problem, updated, http.StatusOK)
	if !updated.GetProblem().GetAllowAiHints() {
		t.Fatal("UpdateProblem did not persist flag")
	}
	g.call(t, f.student, http.MethodPost, path+":hint", req, nil, http.StatusBadRequest)
	activatePro(t, f, f.student)
	created := &v1.CreateSubmissionHintResponse{}
	g.call(t, f.student, http.MethodPost, path+":hint", req, created, http.StatusOK)
	repeated := &v1.CreateSubmissionHintResponse{}
	g.call(t, f.student, http.MethodPost, path+":hint", req, repeated, http.StatusOK)
	if !proto.Equal(created, repeated) || calls.Load() != 1 {
		t.Fatal("gateway replay called provider")
	}
	for _, who := range []auth.Principal{f.student, f.author} {
		history := &v1.ListSubmissionHintsResponse{}
		g.call(t, who, http.MethodGet, path+"/hints", nil, history, http.StatusOK)
		if len(history.GetHints()) != 1 || !proto.Equal(history.Hints[0], created.Hint) {
			t.Fatal("gateway history mismatch")
		}
	}
	g.call(t, f.student2, http.MethodGet, path+"/hints", nil, nil, http.StatusNotFound)
	g.call(t, f.student, http.MethodGet, path+"/hints?page_size=101", nil, nil, http.StatusBadRequest)
	g.call(t, f.student, http.MethodGet, path+"/hints?page_token=bad", nil, nil, http.StatusBadRequest)
	g.call(t, f.student, http.MethodPost, "/v1/submissions/not-a-uuid:hint", nil, nil, http.StatusBadRequest)
	update.Problem.AllowAiHints = false
	g.call(t, f.author, http.MethodPatch, "/v1/problems/"+f.published.String()+"?update_mask=allow_ai_hints", update.Problem, updated, http.StatusOK)
	g.call(t, f.student, http.MethodPost, path+":hint", req, nil, http.StatusBadRequest)
	t.Log("REST → gRPC → JWT/protovalidate → PostgreSQL → HTTP LLM stub: flag, Pro, ownership, replay and history passed")
}
