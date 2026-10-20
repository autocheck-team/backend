package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
)

const (
	testIssuer   = "http://localhost:8180/realms/autocheck"
	testAudience = "autocheck-api"
)

// keycloakStub подписывает токены своим ключом и отдаёт JWKS, как Keycloak.
type keycloakStub struct {
	signer jose.Signer
	jwks   *httptest.Server
}

func newKeycloakStub(t *testing.T) *keycloakStub {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test-key"))
	if err != nil {
		t.Fatal(err)
	}
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &key.PublicKey, KeyID: "test-key", Algorithm: string(jose.RS256), Use: "sig",
	}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(set)
	}))
	t.Cleanup(srv.Close)
	return &keycloakStub{signer: signer, jwks: srv}
}

func (k *keycloakStub) token(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	now := time.Now()
	c := map[string]any{
		"iss":                testIssuer,
		"aud":                []string{testAudience, "account"},
		"sub":                "0199f1a2-3b4c-7d5e-8f90-123456789abc",
		"exp":                now.Add(5 * time.Minute).Unix(),
		"iat":                now.Unix(),
		"email":              "author@autocheck.local",
		"name":               "Анна Авторова",
		"preferred_username": "author@autocheck.local",
		"realm_access":       map[string]any{"roles": []string{"student", "author", "offline_access"}},
	}
	if mutate != nil {
		mutate(c)
	}
	raw, err := jwt.Signed(k.signer).Claims(c).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestVerify(t *testing.T) {
	kc := newKeycloakStub(t)
	v := NewVerifier(t.Context(), testIssuer, kc.jwks.URL, testAudience)

	p, err := v.Verify(t.Context(), kc.token(t, nil))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if p.ID != uuid.MustParse("0199f1a2-3b4c-7d5e-8f90-123456789abc") {
		t.Errorf("ID = %s", p.ID)
	}
	if p.Name != "Анна Авторова" || p.Email != "author@autocheck.local" {
		t.Errorf("Name/Email = %q/%q", p.Name, p.Email)
	}
	// Служебные роли Keycloak (offline_access и т.п.) отбрасываются.
	if len(p.Roles) != 2 || p.PrimaryRole() != RoleAuthor {
		t.Errorf("Roles = %v, primary = %s", p.Roles, p.PrimaryRole())
	}
}

func TestVerifyNameFallsBackToUsername(t *testing.T) {
	kc := newKeycloakStub(t)
	v := NewVerifier(t.Context(), testIssuer, kc.jwks.URL, testAudience)

	p, err := v.Verify(t.Context(), kc.token(t, func(c map[string]any) { delete(c, "name") }))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "author@autocheck.local" {
		t.Errorf("Name = %q", p.Name)
	}
}

func TestVerifyRejects(t *testing.T) {
	kc := newKeycloakStub(t)
	other := newKeycloakStub(t)
	v := NewVerifier(t.Context(), testIssuer, kc.jwks.URL, testAudience)

	tests := []struct {
		name  string
		token string
	}{
		{"expired", kc.token(t, func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() })},
		{"wrong_audience", kc.token(t, func(c map[string]any) { c["aud"] = []string{"account"} })},
		{"wrong_issuer", kc.token(t, func(c map[string]any) { c["iss"] = "http://evil/realms/autocheck" })},
		{"foreign_key", other.token(t, nil)},
		{"sub_not_uuid", kc.token(t, func(c map[string]any) { c["sub"] = "service-account" })},
		{"garbage", "not.a.jwt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := v.Verify(t.Context(), tt.token); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("want ErrInvalidToken, got %v", err)
			}
		})
	}
}

func TestPrimaryRole(t *testing.T) {
	tests := []struct {
		roles []Role
		want  Role
	}{
		{nil, RoleStudent},
		{[]Role{RoleStudent}, RoleStudent},
		{[]Role{RoleStudent, RoleAuthor}, RoleAuthor},
		{[]Role{RoleAuthor, RoleAdmin, RoleStudent}, RoleAdmin},
	}
	for _, tt := range tests {
		if got := (Principal{Roles: tt.roles}).PrimaryRole(); got != tt.want {
			t.Errorf("PrimaryRole(%v) = %s, want %s", tt.roles, got, tt.want)
		}
	}
}
