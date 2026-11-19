package testgeneration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/autocheck-team/backend/internal/auth"
	"github.com/autocheck-team/backend/internal/billing"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

// Service проверяет права и управляет принятием черновиков.
type Service struct {
	v1.UnimplementedTestGenerationServiceServer
	repo      *Repository
	objects   Objects
	validator protovalidate.Validator
	enabled   bool
}

// New собирает API; выключенная LLM блокирует только создание новых генераций.
func New(repo *Repository, objects Objects, validator protovalidate.Validator, enabled bool) *Service {
	return &Service{repo: repo, objects: objects, validator: validator, enabled: enabled}
}

func (s *Service) access(ctx context.Context, problemID string, message proto.Message) (auth.Principal, error) {
	user, err := auth.Require(ctx, auth.RoleAuthor, auth.RoleAdmin)
	if err != nil {
		return user, err
	}
	if err := s.validator.Validate(message); err != nil {
		return user, status.Error(codes.InvalidArgument, "invalid test generation request")
	}
	author, err := s.repo.Author(ctx, uuid.MustParse(problemID))
	if err != nil {
		return user, mapError(err)
	}
	if author != user.ID && !user.Has(auth.RoleAdmin) {
		return user, mapError(ErrNotFound)
	}
	return user, nil
}

// CreateTestGeneration сохраняет снимок и резерв квоты до начала внешних вызовов.
func (s *Service) CreateTestGeneration(ctx context.Context, req *v1.CreateTestGenerationRequest) (*v1.CreateTestGenerationResponse, error) {
	user, err := s.access(ctx, req.GetProblemId(), req)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetSource()) == "" {
		return nil, status.Error(codes.InvalidArgument, "empty reference source")
	}
	if !s.enabled {
		return nil, status.Error(codes.Unavailable, "AI functions are disabled")
	}
	g, err := s.repo.Create(ctx, Generation{ProblemID: uuid.MustParse(req.GetProblemId()), UserID: user.ID, RequestID: uuid.MustParse(req.GetRequestId())}, Task{
		Source: req.GetSource(), Wishes: req.GetWishes(), TestCount: req.GetTestCount(),
	}, req.GetLanguageId())
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.CreateTestGenerationResponse{Generation: toProto(g)}, nil
}

// GetTestGeneration разрешён владельцу задания и администратору независимо от текущего тарифа.
func (s *Service) GetTestGeneration(ctx context.Context, req *v1.GetTestGenerationRequest) (*v1.GetTestGenerationResponse, error) {
	if _, err := s.access(ctx, req.GetProblemId(), req); err != nil {
		return nil, err
	}
	g, err := s.repo.Get(ctx, uuid.MustParse(req.GetProblemId()), uuid.MustParse(req.GetId()))
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.GetTestGenerationResponse{Generation: toProto(g)}, nil
}

// AcceptTestGeneration сначала проверяет весь выбор и сохраняет файлы, затем атомарно включает тесты.
func (s *Service) AcceptTestGeneration(ctx context.Context, req *v1.AcceptTestGenerationRequest) (*v1.AcceptTestGenerationResponse, error) {
	if _, err := s.access(ctx, req.GetProblemId(), req); err != nil {
		return nil, err
	}
	g, err := s.repo.Get(ctx, uuid.MustParse(req.GetProblemId()), uuid.MustParse(req.GetId()))
	if err != nil {
		return nil, mapError(err)
	}
	if g.Status != StatusDone {
		return nil, mapError(ErrSelection)
	}
	selected := make(map[uuid.UUID]Draft, len(g.Tests))
	for _, draft := range g.Tests {
		if draft.RejectionReason == "" {
			selected[draft.ID] = draft
		}
	}
	ids := make([]uuid.UUID, 0, len(req.GetTestIds()))
	for _, value := range req.GetTestIds() {
		id := uuid.MustParse(value)
		if _, ok := selected[id]; !ok {
			return nil, mapError(ErrSelection)
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		draft := selected[id]
		if draft.IsAccepted {
			continue
		}
		for _, file := range []struct {
			name    string
			content []byte
		}{{"input", draft.Input}, {"expected", draft.Expected}} {
			uploadCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			key := fmt.Sprintf("problems/%s/tests/%s/%s", g.ProblemID, id, file.name)
			err := s.objects.Put(uploadCtx, key, bytes.NewReader(file.content), int64(len(file.content)), "application/octet-stream")
			cancel()
			if err != nil {
				return nil, fmt.Errorf("store generated test: %w", err)
			}
		}
	}
	if err := s.repo.Accept(ctx, g, ids); err != nil {
		return nil, mapError(err)
	}
	g, err = s.repo.Get(ctx, g.ProblemID, g.ID)
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.AcceptTestGenerationResponse{Generation: toProto(g)}, nil
}

func mapError(err error) error {
	var limit *billing.GenerationLimitError
	switch {
	case errors.Is(err, ErrNotFound):
		return status.Error(codes.NotFound, "problem or generation not found")
	case errors.Is(err, ErrSelection):
		return status.Error(codes.FailedPrecondition, "select valid drafts from a completed generation")
	case errors.Is(err, ErrLanguage):
		return status.Error(codes.InvalidArgument, "language is not active for problem")
	case errors.Is(err, billing.ErrProRequired):
		return status.Error(codes.FailedPrecondition, "active Pro subscription required")
	case errors.As(err, &limit):
		result, detailErr := status.New(codes.ResourceExhausted, "daily test generation limit exhausted").WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(limit.RetryAfter)})
		if detailErr != nil {
			return fmt.Errorf("encode retry detail: %w", detailErr)
		}
		return result.Err()
	default:
		return err
	}
}

func toProto(g Generation) *v1.TestGeneration {
	result := &v1.TestGeneration{
		Id: g.ID.String(), ProblemId: g.ProblemID.String(), Status: statusToProto(g.Status), Error: g.Error,
		CreateTime: timestamppb.New(g.CreatedAt), UpdateTime: timestamppb.New(g.UpdatedAt), Tests: make([]*v1.GeneratedTest, 0, len(g.Tests)),
	}
	for _, draft := range g.Tests {
		result.Tests = append(result.Tests, &v1.GeneratedTest{Id: draft.ID.String(), Input: draft.Input, Expected: draft.Expected, Explanation: draft.Explanation, RejectionReason: draft.RejectionReason, IsAccepted: draft.IsAccepted})
	}
	return result
}

func statusToProto(value Status) v1.TestGenerationStatus {
	switch value {
	case StatusQueued:
		return v1.TestGenerationStatus_TEST_GENERATION_STATUS_QUEUED
	case StatusRunning:
		return v1.TestGenerationStatus_TEST_GENERATION_STATUS_RUNNING
	case StatusDone:
		return v1.TestGenerationStatus_TEST_GENERATION_STATUS_DONE
	case StatusFailed:
		return v1.TestGenerationStatus_TEST_GENERATION_STATUS_FAILED
	default:
		return v1.TestGenerationStatus_TEST_GENERATION_STATUS_UNSPECIFIED
	}
}
