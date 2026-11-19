// Package playground хранит личные проекты и исполняет неизменяемые снимки в песочнице.
package playground

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound скрывает отсутствие ресурса и отсутствие доступа одинаково.
var ErrNotFound = errors.New("project or run not found")

// ErrProjectLimit сообщает о заполненном личном пространстве.
var ErrProjectLimit = errors.New("project limit exhausted")

// ErrLanguage означает неизвестный или выключенный язык.
var ErrLanguage = errors.New("language is not active")

// ErrFiles означает некорректный набор файлов для выбранного языка.
var ErrFiles = errors.New("invalid project files")

// ErrRequestConflict означает повтор ключа запуска для другого проекта.
var ErrRequestConflict = errors.New("request id belongs to another project")

// ErrLeaseLost запрещает запись от устаревшей попытки воркера.
var ErrLeaseLost = errors.New("playground lease lost")

const (
	maxProjects  = 20
	maxFiles     = 20
	maxFileBytes = 256 << 10
	outputBytes  = 64 << 10
)

// File — текстовый файл в корне рабочего каталога.
type File struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// Project — сохранённый набор файлов пользователя.
type Project struct {
	ID, UserID, RequestID uuid.UUID
	Title, LanguageID     string
	Files                 []File
	CreatedAt, UpdatedAt  time.Time
}

// Language — снимок доверенных команд из каталога languages.
type Language struct {
	Image, SourceFile              string
	CompileCmd, RunCmd, CompileEnv []string
}

// Status описывает жизненный цикл запуска независимо от protobuf.
type Status int32

// Состояния запуска; ноль не сохраняется.
const (
	StatusUnspecified Status = iota
	StatusQueued
	StatusCompiling
	StatusRunning
	StatusDone
	StatusFailed
)

// Run — снимок проекта, ввод и результат запуска.
type Run struct {
	ID, ProjectID, UserID, RequestID uuid.UUID
	LanguageID                       string
	Language                         Language
	Files                            []File
	Stdin                            string
	Status                           Status
	Result
	CreatedAt, UpdatedAt time.Time
}

// Result содержит ограниченный вывод и измерения песочницы.
type Result struct {
	Verdict                    string
	ExitCode                   *int32
	Stdout, Stderr, CompileLog string
	Truncated                  bool
	TimeMS, MemoryKB           int64
}

// Job связывает запуск с поколением аренды воркера.
type Job struct {
	ID       int64
	RunID    uuid.UUID
	Owner    string
	Attempts int
}
