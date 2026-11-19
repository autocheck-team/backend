package playground

import (
	"context"

	"github.com/google/uuid"
)

// Store — операции хранилища, потребляемые сценариями API.
type Store interface {
	Create(ctx context.Context, project Project) (Project, error)
	Get(ctx context.Context, id uuid.UUID) (Project, error)
	List(ctx context.Context, userID, after uuid.UUID, limit int) ([]Project, error)
	Update(ctx context.Context, project Project) (Project, error)
	Delete(ctx context.Context, id, userID uuid.UUID) error
	Enqueue(ctx context.Context, projectID, userID, requestID uuid.UUID, stdin string) (Run, error)
	GetRun(ctx context.Context, id uuid.UUID) (Run, error)
}
