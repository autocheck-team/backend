// Package material предоставляет учебные материалы платформы.
package material

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound означает отсутствие материала или вложения.
var ErrNotFound = errors.New("material record not found")

// ErrUnknownProblem означает ссылку на несуществующее задание.
var ErrUnknownProblem = errors.New("unknown material problem")

// Material хранит текст и привязки учебного материала.
type Material struct {
	ID              uuid.UUID
	RequestID       uuid.UUID
	Title           string
	Description     string
	ContentMarkdown string
	ProblemIDs      []uuid.UUID
	Ordinal         int32
	IsPublished     bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Attachment хранит проверенные метаданные файла и закрытый ключ S3.
type Attachment struct {
	ID          uuid.UUID
	MaterialID  uuid.UUID
	RequestID   uuid.UUID
	Filename    string
	ObjectKey   string
	ContentType string
	SizeBytes   int64
	IsReady     bool
}

// ListFilter задаёт область видимости и keyset по паре ordinal, id.
type ListFilter struct {
	ProblemID uuid.UUID
	All       bool
	After     uuid.UUID
	Ordinal   int32
	Limit     int
}
