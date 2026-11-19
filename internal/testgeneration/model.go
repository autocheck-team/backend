// Package testgeneration создаёт проверенные эталоном черновики тестов независимо от очереди посылок.
package testgeneration

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/autocheck-team/backend/internal/llm"
)

const maxData = 1 << 20

// Language — снимок доверенных команд тулчейна из БД.
type Language struct {
	ID, Image, SourceFile          string
	CompileCmd, RunCmd, CompileEnv []string
}

// Task хранит данные автора отдельно от системных инструкций модели.
type Task struct {
	Statement, Source, Wishes  string
	TestCount                  int32
	TimeLimitMS, MemoryLimitKB int32
	Language                   Language
}

// Draft — проверенный кандидат либо безопасная причина отбрасывания.
type Draft struct {
	ID                           uuid.UUID
	Input, Expected              []byte
	Explanation, RejectionReason string
	IsAccepted                   bool
}

// Status описывает этап обработки независимо от protobuf.
type Status int32

// Этапы генерации соответствуют ограничению status в БД.
const (
	StatusUnspecified Status = iota
	StatusQueued
	StatusRunning
	StatusDone
	StatusFailed
)

// Generation — сохранённое состояние очереди и кандидатов.
type Generation struct {
	ID, ProblemID, UserID, RequestID uuid.UUID
	Status                           Status
	Error                            string
	CreatedAt, UpdatedAt             time.Time
	Tests                            []Draft
}

// Job содержит уникальное поколение аренды и снимок задачи.
type Job struct {
	ID, Token uuid.UUID
	Task      Task
	Attempts  int
}

// Model задаётся потребителем для подмены только внешнего LLM.
type Model interface {
	Chat(ctx context.Context, messages []llm.Message, options llm.Options) (json.RawMessage, error)
}

// Objects сохраняет файлы по устойчивым ключам до атомарного принятия в БД.
type Objects interface {
	Put(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error
}
