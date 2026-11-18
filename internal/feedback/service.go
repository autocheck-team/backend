package feedback

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/autocheck-team/backend/internal/auth"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

const defaultPageSize = 20

// Service реализует FeedbackService с проверкой доступа и переходов статуса.
type Service struct {
	v1.UnimplementedFeedbackServiceServer
	repo      *Repository
	validator protovalidate.Validator
}

// NewService создаёт сервис запросов пользователей.
func NewService(repo *Repository, validator protovalidate.Validator) *Service {
	return &Service{repo: repo, validator: validator}
}

// CreateFeedbackRequest сохраняет запрос с ключом идемпотентности в пределах автора.
func (s *Service) CreateFeedbackRequest(ctx context.Context, req *v1.CreateFeedbackRequestRequest) (*v1.CreateFeedbackRequestResponse, error) {
	user, err := auth.Require(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.validate(req); err != nil {
		return nil, err
	}
	var requestID *uuid.UUID
	if req.GetClientRequestId() != "" {
		id := uuid.MustParse(req.GetClientRequestId())
		requestID = &id
		existing, err := s.repo.FindByClientRequest(ctx, user.ID, id)
		if err == nil {
			return &v1.CreateFeedbackRequestResponse{FeedbackRequest: toProto(existing)}, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("find previous feedback request: %w", err)
		}
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("generate feedback request id: %w", err)
	}
	created, err := s.repo.Create(ctx, Request{
		ID: id, UserID: user.ID, Kind: kindFromProto(req.GetType()), LanguageID: req.LanguageId,
		Title: req.GetTitle(), Text: req.GetText(), ClientRequestID: requestID,
	})
	if errors.Is(err, errDuplicateRequest) && requestID != nil {
		created, err = s.repo.FindByClientRequest(ctx, user.ID, *requestID)
	}
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.CreateFeedbackRequestResponse{FeedbackRequest: toProto(created)}, nil
}

// GetFeedbackRequest скрывает чужой запрос от всех, кроме администратора.
func (s *Service) GetFeedbackRequest(ctx context.Context, req *v1.GetFeedbackRequestRequest) (*v1.GetFeedbackRequestResponse, error) {
	user, err := auth.Require(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.validate(req); err != nil {
		return nil, err
	}
	request, err := s.repo.Get(ctx, uuid.MustParse(req.GetId()))
	if err != nil {
		return nil, mapError(err)
	}
	if request.UserID != user.ID && !user.Has(auth.RoleAdmin) {
		return nil, mapError(ErrNotFound)
	}
	return &v1.GetFeedbackRequestResponse{FeedbackRequest: toProto(request)}, nil
}

// ListFeedbackRequests ограничивает выборку автором до применения пагинации.
func (s *Service) ListFeedbackRequests(ctx context.Context, req *v1.ListFeedbackRequestsRequest) (*v1.ListFeedbackRequestsResponse, error) {
	user, err := auth.Require(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.validate(req); err != nil {
		return nil, err
	}
	limit := int(req.GetPageSize())
	if limit == 0 {
		limit = defaultPageSize
	}
	filter := ListFilter{Kind: kindFromProto(req.GetType()), Status: statusFromProto(req.GetStatus()), Limit: limit + 1}
	if !user.Has(auth.RoleAdmin) {
		filter.UserID = user.ID
	}
	if req.GetPageToken() != "" {
		data, err := base64.RawURLEncoding.DecodeString(req.GetPageToken())
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "invalid page_token")
		}
		filter.After, err = uuid.FromBytes(data)
		if err != nil || filter.After == uuid.Nil {
			return nil, status.Error(codes.InvalidArgument, "invalid page_token")
		}
	}
	requests, err := s.repo.List(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("list visible feedback requests: %w", err)
	}
	resp := &v1.ListFeedbackRequestsResponse{}
	if len(requests) > limit {
		requests = requests[:limit]
		last := requests[limit-1].ID
		resp.NextPageToken = base64.RawURLEncoding.EncodeToString(last[:])
	}
	resp.FeedbackRequests = make([]*v1.FeedbackRequest, 0, len(requests))
	for _, request := range requests {
		resp.FeedbackRequests = append(resp.FeedbackRequests, toProto(request))
	}
	return resp, nil
}

// UpdateFeedbackRequest применяет разрешённый переход с защитой от параллельного изменения.
func (s *Service) UpdateFeedbackRequest(ctx context.Context, req *v1.UpdateFeedbackRequestRequest) (*v1.UpdateFeedbackRequestResponse, error) {
	if _, err := auth.Require(ctx, auth.RoleAdmin); err != nil {
		return nil, err
	}
	if err := s.validate(req); err != nil {
		return nil, err
	}
	id := uuid.MustParse(req.GetId())
	previous, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, mapError(err)
	}
	next := statusFromProto(req.GetStatus())
	if !canTransition(previous.Status, next) {
		return nil, status.Error(codes.FailedPrecondition, "feedback request status transition is not allowed")
	}
	updated, err := s.repo.Update(ctx, id, previous.Status, next, req.GetAdminComment())
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.UpdateFeedbackRequestResponse{FeedbackRequest: toProto(updated)}, nil
}

func canTransition(previous, next Status) bool {
	if previous == next {
		return previous >= StatusNew && previous <= StatusCompleted
	}
	switch previous {
	case StatusNew:
		return next == StatusAccepted || next == StatusRejected
	case StatusAccepted:
		return next == StatusCompleted || next == StatusRejected
	default:
		return false
	}
}

func (s *Service) validate(message proto.Message) error {
	// Проверяем также прямые вызовы сервиса: некорректный UUID не должен приводить к panic.
	if err := s.validator.Validate(message); err != nil {
		return status.Error(codes.InvalidArgument, "invalid feedback request")
	}
	return nil
}

func mapError(err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return status.Error(codes.NotFound, "feedback request not found")
	case errors.Is(err, ErrUnknownLanguage):
		return status.Error(codes.InvalidArgument, "unknown language_id")
	case errors.Is(err, ErrConflict):
		return status.Error(codes.Aborted, "feedback request changed; retry the update")
	default:
		return err
	}
}
