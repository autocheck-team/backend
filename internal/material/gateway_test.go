package material

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
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "material-test"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &key.PublicKey, KeyID: "material-test", Algorithm: string(jose.RS256), Use: "sig",
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
	const issuer = "http://material.test/realms/autocheck"
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
		Submission: &v1.UnimplementedSubmissionServiceServer{}, Feedback: &v1.UnimplementedFeedbackServiceServer{}, Material: f.svc,
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

func TestGatewayMaterialLifecycle(t *testing.T) {
	// arrange
	f := setup(t)
	g := startGateway(t, f)
	const path = "/v1/materials"
	request := &v1.CreateMaterialRequest{Material: f.input(), RequestId: uuid.NewString()}
	request.Material.IsPublished = false
	created := &v1.CreateMaterialResponse{}
	// act
	g.call(t, f.admin, http.MethodPost, path, request, created, http.StatusOK)
	// assert
	id := created.GetMaterial().GetId()
	if id == "" || created.GetMaterial().GetContentMarkdown() == "" {
		t.Fatalf("created=%v", created)
	}
	g.call(t, auth.Principal{}, http.MethodGet, path, nil, nil, http.StatusUnauthorized)
	g.call(t, f.student, http.MethodPost, path, request, nil, http.StatusForbidden)
	g.call(t, f.author, http.MethodPut, path+"/"+id, &v1.UpdateMaterialRequest{Material: f.input()}, nil, http.StatusForbidden)
	g.call(t, f.other, http.MethodGet, path+"/"+id, nil, nil, http.StatusNotFound)
	list := &v1.ListMaterialsResponse{}
	g.call(t, f.student, http.MethodGet, path, nil, list, http.StatusOK)
	if len(list.GetMaterials()) != 0 {
		t.Fatal("draft visible")
	}
	updated := &v1.UpdateMaterialResponse{}
	g.call(t, f.admin, http.MethodPut, path+"/"+id, &v1.UpdateMaterialRequest{Material: f.input()}, updated, http.StatusOK)
	get := &v1.GetMaterialResponse{}
	g.call(t, f.student, http.MethodGet, path+"/"+id, nil, get, http.StatusOK)
	if !get.GetMaterial().GetIsLocked() || get.GetMaterial().GetContentMarkdown() != "" || get.GetMaterial().GetDescription() == "" {
		t.Fatalf("locked=%v", get)
	}
	f.subscribe(t, f.other, "active", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	g.call(t, f.other, http.MethodGet, path+"/"+id, nil, get, http.StatusOK)
	if get.GetMaterial().GetIsLocked() || get.GetMaterial().GetContentMarkdown() == "" {
		t.Fatalf("pro=%v", get)
	}
	f.create(t, true, 4)
	g.call(t, f.student, http.MethodGet, "/v1/problems/"+f.problemID.String()+"/materials?page_size=1", nil, list, http.StatusOK)
	if len(list.GetMaterials()) != 1 || list.GetNextPageToken() == "" || list.GetMaterials()[0].GetContentMarkdown() != "" {
		t.Fatalf("page=%v", list)
	}
	token := list.GetNextPageToken()
	g.call(t, f.student, http.MethodGet, path+"?page_size=1&problem_id="+f.problemID.String()+"&page_token="+token, nil, list, http.StatusOK)
	if len(list.GetMaterials()) != 1 || list.GetMaterials()[0].GetId() == id || list.GetNextPageToken() != "" {
		t.Fatalf("next=%v", list)
	}
	g.call(t, f.student, http.MethodGet, path+"?page_token="+token, nil, nil, http.StatusBadRequest)
	g.call(t, f.student, http.MethodGet, path+"?page_size=101", nil, nil, http.StatusBadRequest)
	g.call(t, f.admin, http.MethodPut, path+"/"+id, &v1.UpdateMaterialRequest{}, nil, http.StatusBadRequest)
	g.call(t, f.student, http.MethodGet, path+"/bad-id", nil, nil, http.StatusBadRequest)
	upload := &v1.CreateMaterialAttachmentResponse{}
	g.call(t, f.admin, http.MethodPost, path+"/"+id+"/attachments", &v1.CreateMaterialAttachmentRequest{Filename: "lesson.pdf", RequestId: uuid.NewString()}, upload, http.StatusOK)
	attachmentID := upload.GetAttachment().GetId()
	f.objects.upload(attachmentPrefix(uuid.MustParse(id), uuid.MustParse(attachmentID))+"upload", []byte("%PDF-1.7\nlesson"))
	confirmed := &v1.ConfirmMaterialAttachmentResponse{}
	g.call(t, f.admin, http.MethodPost, path+"/"+id+"/attachments/"+attachmentID+":confirm", &v1.ConfirmMaterialAttachmentRequest{}, confirmed, http.StatusOK)
	g.call(t, f.student, http.MethodGet, path+"/"+id, nil, get, http.StatusOK)
	if len(get.GetMaterial().GetAttachments()) != 0 {
		t.Fatal("free links leaked")
	}
	g.call(t, f.other, http.MethodGet, path+"/"+id, nil, get, http.StatusOK)
	if len(get.GetMaterial().GetAttachments()) != 1 || get.GetMaterial().GetAttachments()[0].GetDownloadUrl() == "" {
		t.Fatalf("pro links=%v", get)
	}
	g.call(t, f.student, http.MethodDelete, path+"/"+id, nil, nil, http.StatusForbidden)
	g.call(t, f.admin, http.MethodDelete, path+"/"+id, nil, &v1.DeleteMaterialResponse{}, http.StatusOK)
	g.call(t, f.admin, http.MethodGet, path+"/"+id, nil, nil, http.StatusNotFound)
	t.Log("REST: JWT, CRUD, draft visibility, free/Pro access, keyset, attachment confirm and validation passed")
}
