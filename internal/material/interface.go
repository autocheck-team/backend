package material

import (
	"context"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/autocheck-team/backend/internal/billing"
)

// Store объявляет необходимые сценарию операции с материалами.
type Store interface {
	Create(context.Context, Material) (Material, error)
	Update(context.Context, Material) (Material, error)
	Get(context.Context, uuid.UUID) (Material, error)
	List(context.Context, ListFilter) ([]Material, error)
	Delete(context.Context, uuid.UUID) error
	CreateAttachment(context.Context, Attachment) (Attachment, error)
	Attachment(context.Context, uuid.UUID, uuid.UUID) (Attachment, error)
	Attachments(context.Context, uuid.UUID) ([]Attachment, error)
	ConfirmAttachment(context.Context, Attachment) (Attachment, error)
	DeleteAttachment(context.Context, uuid.UUID, uuid.UUID) error
}

// Entitlements предоставляет эффективный тариф из billing с проверкой срока подписки.
type Entitlements interface {
	Limits(context.Context, uuid.UUID) (billing.Limits, error)
}

// Objects предоставляет операции S3 для загрузки и фиксации проверенного файла.
type Objects interface {
	PresignPut(context.Context, string, time.Duration) (string, error)
	PresignGet(context.Context, string, time.Duration) (string, error)
	Get(context.Context, string) (io.ReadCloser, error)
	Put(context.Context, string, io.Reader, int64, string) error
	DeletePrefix(context.Context, string) error
}
