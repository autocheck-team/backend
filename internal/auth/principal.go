// Package auth проверяет access-токены Keycloak и описывает аутентифицированного пользователя.
package auth

import (
	"context"
	"slices"

	"github.com/google/uuid"
)

// Role — роль пользователя на платформе. Значения совпадают с realm roles в Keycloak
// и с типом user_role в БД.
type Role string

// Роли платформы.
const (
	RoleStudent Role = "student"
	RoleAuthor  Role = "author"
	RoleAdmin   Role = "admin"
)

// Principal — пользователь, от имени которого выполняется запрос.
type Principal struct {
	ID    uuid.UUID
	Email string
	Name  string
	Roles []Role
}

// Has сообщает, есть ли у пользователя хотя бы одна из ролей.
func (p Principal) Has(roles ...Role) bool {
	for _, r := range roles {
		if slices.Contains(p.Roles, r) {
			return true
		}
	}
	return false
}

// PrimaryRole — старшая роль пользователя: admin > author > student.
func (p Principal) PrimaryRole() Role {
	switch {
	case p.Has(RoleAdmin):
		return RoleAdmin
	case p.Has(RoleAuthor):
		return RoleAuthor
	default:
		return RoleStudent
	}
}

type ctxKey struct{}

// WithPrincipal кладёт пользователя в контекст запроса.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext достаёт пользователя из контекста. ok == false, если запрос не аутентифицирован.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}
