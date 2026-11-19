package submission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/autocheck-team/backend/internal/auth"
	"github.com/autocheck-team/backend/internal/billing"
	"github.com/autocheck-team/backend/internal/llm"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

// HintModel отделяет сценарий подсказки от внешнего LLM-провайдера.
type HintModel interface {
	Enabled() bool
	Chat(context.Context, []llm.Message, llm.Options) (json.RawMessage, error)
}

type hintStore interface {
	Problem(context.Context, uuid.UUID) (string, error)
	Get(context.Context, uuid.UUID) (Hint, error)
	Reserve(context.Context, Submission, uuid.UUID, time.Duration) (Hint, error)
	Complete(context.Context, uuid.UUID, uuid.UUID, string, bool) (Hint, error)
	Release(context.Context, uuid.UUID, uuid.UUID) error
}

type hinting struct {
	repo    hintStore
	model   HintModel
	timeout time.Duration
}

// NewWithHints подключает подсказки к существующему API посылок с общим дедлайном из конфига.
func NewWithHints(repo submissions, objects Objects, events *Events, hints hintStore, model HintModel, timeout time.Duration) *Service {
	svc := NewService(repo, objects, events)
	svc.hints = &hinting{repo: hints, model: model, timeout: timeout}
	return svc
}

// CreateSubmissionHint генерирует один ответ к собственной посылке, повтор берётся из истории.
func (s *Service) CreateSubmissionHint(ctx context.Context, req *v1.CreateSubmissionHintRequest) (*v1.CreateSubmissionHintResponse, error) {
	if s.hints != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.hints.timeout)
		defer cancel()
	}
	sub, err := s.hintSubmission(ctx, req.GetId(), true)
	if err != nil {
		return nil, err
	}
	if s.hints == nil {
		return nil, status.Error(codes.Unavailable, "подсказки не настроены")
	}
	statement, err := s.hints.repo.Problem(ctx, sub.ProblemID)
	if err != nil {
		return nil, mapHintError(err)
	}
	existing, err := s.hints.repo.Get(ctx, sub.ID)
	if err == nil {
		return &v1.CreateSubmissionHintResponse{Hint: hintProto(existing)}, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, mapHintError(err)
	}
	if sub.Status != "done" || sub.Verdict == nil {
		return nil, status.Error(codes.FailedPrecondition, "дождитесь завершения проверки")
	}
	if s.hints.model == nil || !s.hints.model.Enabled() {
		return nil, status.Error(codes.Unavailable, "ИИ-подсказки выключены")
	}
	token, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("generate hint lease token: %w", err)
	}
	existing, err = s.hints.repo.Reserve(ctx, sub, token, s.hints.timeout+time.Minute)
	if err != nil {
		return nil, mapHintError(err)
	}
	if existing.Text != "" {
		return &v1.CreateSubmissionHintResponse{Hint: hintProto(existing)}, nil
	}
	hint, err := s.generateHint(ctx, sub, statement, token)
	if err != nil {
		// Отмена клиента не должна оставлять квоту занятой до истечения аренды.
		cleanupErr := s.hints.repo.Release(context.WithoutCancel(ctx), sub.ID, token)
		if cleanupErr != nil {
			return nil, errors.Join(err, cleanupErr)
		}
		return nil, mapHintError(err)
	}
	return &v1.CreateSubmissionHintResponse{Hint: hintProto(hint)}, nil
}

func (s *Service) hintSubmission(ctx context.Context, rawID string, own bool) (Submission, error) {
	user, err := auth.Require(ctx)
	if err != nil {
		return Submission{}, err
	}
	id, err := uuid.Parse(rawID)
	if err != nil {
		return Submission{}, status.Error(codes.InvalidArgument, "некорректный id посылки")
	}
	sub, err := s.loadVisible(ctx, id)
	if err != nil {
		return Submission{}, err
	}
	if own && sub.UserID != user.ID {
		return Submission{}, status.Error(codes.NotFound, "посылка не найдена")
	}
	return sub, nil
}

// ListSubmissionHints сохраняет права GetSubmission и доступ к истории после окончания Pro.
func (s *Service) ListSubmissionHints(ctx context.Context, req *v1.ListSubmissionHintsRequest) (*v1.ListSubmissionHintsResponse, error) {
	if s.hints != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.hints.timeout)
		defer cancel()
	}
	sub, err := s.hintSubmission(ctx, req.GetId(), false)
	if err != nil {
		return nil, err
	}
	if req.GetPageSize() < 0 || req.GetPageSize() > 100 || req.GetPageToken() != "" {
		return nil, status.Error(codes.InvalidArgument, "некорректная страница истории")
	}
	if s.hints == nil {
		return nil, status.Error(codes.Unavailable, "подсказки не настроены")
	}
	hint, err := s.hints.repo.Get(ctx, sub.ID)
	if errors.Is(err, ErrNotFound) {
		return &v1.ListSubmissionHintsResponse{}, nil
	}
	if err != nil {
		return nil, mapHintError(err)
	}
	return &v1.ListSubmissionHintsResponse{Hints: []*v1.SubmissionHint{hintProto(hint)}}, nil
}

func (s *Service) generateHint(ctx context.Context, sub Submission, statement string, token uuid.UUID) (Hint, error) {
	source, err := s.hintSource(ctx, sub.SourceKey)
	if err != nil {
		return Hint{}, err
	}
	runs, err := s.repo.TestRuns(ctx, sub.ID)
	if err != nil {
		return Hint{}, fmt.Errorf("read hint test runs: %w", err)
	}
	// Ограничиваем объём и никогда не читаем объекты закрытых тестов.
	samples := make([]TestRun, 0, min(5, len(runs)))
	for _, run := range runs {
		if run.IsSample && run.Verdict == "WA" && len(samples) < 5 {
			samples = append(samples, run)
		}
	}
	report, err := s.buildReport(ctx, sub, samples)
	if err != nil {
		return Hint{}, fmt.Errorf("build hint report: %w", err)
	}
	messages, err := hintMessages(statement, source, report)
	if err != nil {
		return Hint{}, err
	}
	text, fallback, err := requestHint(ctx, s.hints.model, messages)
	if err != nil {
		return Hint{}, err
	}
	return s.hints.repo.Complete(ctx, sub.ID, token, text, fallback)
}

func (s *Service) hintSource(ctx context.Context, key string) (source string, err error) {
	reader, err := s.objects.Get(ctx, key)
	if err != nil {
		return "", fmt.Errorf("get hint source: %w", err)
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close hint source: %w", closeErr))
		}
	}()
	data, err := io.ReadAll(io.LimitReader(reader, maxSourceBytes+1))
	if err != nil {
		return "", fmt.Errorf("read hint source: %w", err)
	}
	if len(data) > maxSourceBytes {
		return "", errors.New("hint source exceeds size limit")
	}
	return strings.ToValidUTF8(string(data), "\uFFFD"), nil
}

func hintProto(hint Hint) *v1.SubmissionHint {
	return &v1.SubmissionHint{Id: hint.ID, Text: hint.Text, IsFallback: hint.IsFallback, CreateTime: timestamppb.New(hint.CreatedAt)}
}

func mapHintError(err error) error {
	var limit *billing.HintLimitError
	switch {
	case errors.Is(err, errHintsDisabled):
		return status.Error(codes.FailedPrecondition, "автор запретил ИИ-подсказки")
	case errors.Is(err, billing.ErrProRequired):
		return status.Error(codes.FailedPrecondition, "нужна активная подписка Pro")
	case errors.As(err, &limit):
		result, detailErr := status.New(codes.ResourceExhausted, "дневной лимит подсказок исчерпан").WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(limit.RetryAfter)})
		if detailErr != nil {
			return fmt.Errorf("encode hint retry detail: %w", detailErr)
		}
		return result.Err()
	case errors.Is(err, errHintInProgress), errors.Is(err, errHintLeaseLost):
		return status.Error(codes.Aborted, "подсказка уже создаётся, повторите запрос")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return status.FromContextError(err).Err()
	case errors.Is(err, llm.ErrDisabled):
		return status.Error(codes.Unavailable, "ИИ-подсказки выключены")
	default:
		return err
	}
}
