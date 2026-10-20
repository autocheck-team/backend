package user

import (
	"context"

	"github.com/autocheck-team/backend/internal/auth"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

// Service реализует UserService.
type Service struct {
	v1.UnimplementedUserServiceServer
}

// NewService создаёт Service.
func NewService() *Service {
	return &Service{}
}

// GetMe возвращает профиль пользователя из токена. В БД он уже записан
// интерсептором авторизации (Syncer).
func (s *Service) GetMe(ctx context.Context, _ *v1.GetMeRequest) (*v1.GetMeResponse, error) {
	p, err := auth.Require(ctx)
	if err != nil {
		return nil, err
	}
	u := &v1.User{Id: p.ID.String(), Email: p.Email, DisplayName: p.Name}
	for _, r := range p.Roles {
		u.Roles = append(u.Roles, RoleToProto(r))
	}
	return &v1.GetMeResponse{User: u}, nil
}

// RoleToProto переводит роль в значение API.
func RoleToProto(r auth.Role) v1.Role {
	switch r {
	case auth.RoleStudent:
		return v1.Role_ROLE_STUDENT
	case auth.RoleAuthor:
		return v1.Role_ROLE_AUTHOR
	case auth.RoleAdmin:
		return v1.Role_ROLE_ADMIN
	default:
		return v1.Role_ROLE_UNSPECIFIED
	}
}
