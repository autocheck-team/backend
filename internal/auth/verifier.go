package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
)

// ErrInvalidToken — токен не прошёл проверку: подпись, срок, issuer или audience.
var ErrInvalidToken = errors.New("invalid token")

// Verifier проверяет access-токены Keycloak по его публичным ключам (JWKS).
type Verifier struct {
	v *oidc.IDTokenVerifier
}

// NewVerifier создаёт проверку токенов.
//
// issuer — ожидаемый claim iss (адрес realm, каким его видит браузер), jwksURL — откуда
// брать ключи (внутри compose это другой хост, поэтому discovery по issuer не подходит),
// audience — ожидаемый claim aud. Ключи загружаются лениво при первой проверке и
// обновляются, когда в токене встречается неизвестный kid.
func NewVerifier(ctx context.Context, issuer, jwksURL, audience string) *Verifier {
	keys := oidc.NewRemoteKeySet(ctx, jwksURL)
	return &Verifier{v: oidc.NewVerifier(issuer, keys, &oidc.Config{ClientID: audience})}
}

type claims struct {
	Email             string `json:"email"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	RealmAccess       struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

// Verify проверяет токен и возвращает пользователя.
func (v *Verifier) Verify(ctx context.Context, raw string) (Principal, error) {
	tok, err := v.v.Verify(ctx, raw)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	id, err := uuid.Parse(tok.Subject)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: sub is not uuid: %w", ErrInvalidToken, err)
	}
	var c claims
	if err := tok.Claims(&c); err != nil {
		return Principal{}, fmt.Errorf("%w: decode claims: %w", ErrInvalidToken, err)
	}

	p := Principal{ID: id, Email: c.Email, Name: strings.TrimSpace(c.Name)}
	if p.Name == "" {
		p.Name = c.PreferredUsername
	}
	for _, r := range c.RealmAccess.Roles {
		switch role := Role(r); role {
		case RoleStudent, RoleAuthor, RoleAdmin:
			p.Roles = append(p.Roles, role)
		}
	}
	return p, nil
}
