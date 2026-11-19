package playground

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
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "playground-test"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &key.PublicKey, KeyID: "playground-test", Algorithm: string(jose.RS256), Use: "sig",
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
	const issuer = "http://playground.test/realms/autocheck"
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
		Submission: &v1.UnimplementedSubmissionServiceServer{}, Playground: f.svc, Feedback: &v1.UnimplementedFeedbackServiceServer{},
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

func TestGatewayCRUD(t *testing.T) {
	f := setup(t)
	g := startGateway(t, f)
	request := validCreate()
	var created v1.CreateProjectResponse
	g.call(t, auth.Principal{}, "POST", "/v1/projects", request, nil, 401)
	invalid := validCreate()
	invalid.Files[0].Name = "../escape"
	g.call(t, f.student, "POST", "/v1/projects", invalid, nil, 400)
	g.call(t, f.student, "POST", "/v1/projects", request, &created, 200)
	path := "/v1/projects/" + created.Project.Id
	g.call(t, f.other, "GET", path, nil, nil, 404)
	g.call(t, f.admin, "GET", path, nil, &v1.GetProjectResponse{}, 200)
	update := &v1.UpdateProjectRequest{Id: created.Project.Id, Title: "updated", LanguageId: "python3.14", Files: created.Project.Files}
	g.call(t, f.admin, "PUT", path, update, nil, 404)
	g.call(t, f.student, "PUT", path, update, &v1.UpdateProjectResponse{}, 200)
	var listed v1.ListProjectsResponse
	g.call(t, f.student, "GET", "/v1/projects?page_size=1", nil, &listed, 200)
	if len(listed.Projects) != 1 || listed.Projects[0].Title != "updated" {
		t.Fatal("updated project missing")
	}
	g.call(t, f.student, "GET", "/v1/projects?page_size=101", nil, nil, 400)
	g.call(t, f.student, "GET", "/v1/projects/not-a-uuid", nil, nil, 400)
	g.call(t, f.other, "DELETE", path, nil, nil, 404)
	g.call(t, f.student, "DELETE", path, nil, &v1.DeleteProjectResponse{}, 200)
	g.call(t, f.student, "GET", path, nil, nil, 404)
}
