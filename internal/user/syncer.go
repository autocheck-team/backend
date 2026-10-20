// Package user — профиль пользователя: локальная копия данных из Keycloak и UserService.
package user

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/autocheck-team/backend/internal/auth"
)

// Syncer записывает профиль пользователя из токена в таблицу users.
//
// Запись нужна, чтобы задания и посылки ссылались на пользователя внешним ключом.
// Чтобы не писать в БД на каждый запрос, Syncer помнит последний записанный профиль
// и обращается к базе, только когда он поменялся: новый пользователь, другое имя или роль.
type Syncer struct {
	pool *pgxpool.Pool
	seen sync.Map // uuid.UUID -> profile
}

type profile struct {
	email, name string
	role        auth.Role
}

// NewSyncer создаёт Syncer.
func NewSyncer(pool *pgxpool.Pool) *Syncer {
	return &Syncer{pool: pool}
}

// Sync создаёт или обновляет пользователя.
func (s *Syncer) Sync(ctx context.Context, p auth.Principal) error {
	cur := profile{email: p.Email, name: p.Name, role: p.PrimaryRole()}
	if prev, ok := s.seen.Load(p.ID); ok && prev.(profile) == cur {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO users (id, email, display_name, role)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE
		SET email = EXCLUDED.email, display_name = EXCLUDED.display_name,
		    role = EXCLUDED.role, updated_at = now()
		WHERE (users.email, users.display_name, users.role)
		      IS DISTINCT FROM (EXCLUDED.email, EXCLUDED.display_name, EXCLUDED.role)`,
		p.ID, cur.email, cur.name, string(cur.role))
	if err != nil {
		return fmt.Errorf("sync user %s: %w", p.ID, err)
	}
	s.seen.Store(p.ID, cur)
	return nil
}
