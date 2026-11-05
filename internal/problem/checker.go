package problem

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/autocheck-team/backend/internal/storage"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

// GetCheckerUpload выдаёт ссылку только владельцу: студент не может подменить чекер.
func (s *Service) GetCheckerUpload(ctx context.Context, req *v1.GetCheckerUploadRequest) (*v1.GetCheckerUploadResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, p, err := s.loadEditable(ctx, uuid.MustParse(req.GetProblemId()))
	if err != nil {
		return nil, err
	}
	if p.Checker != "custom" {
		return nil, status.Error(codes.FailedPrecondition, "задание не использует custom-чекер")
	}
	if p.IsPublished {
		return nil, status.Error(codes.FailedPrecondition, "сначала снимите задание с публикации")
	}
	if err := s.repo.SetCheckerReady(ctx, p.ID, false); err != nil {
		return nil, err
	}
	url, err := s.objects.PresignPut(ctx, *p.CheckerKey, uploadURLTTL)
	if err != nil {
		return nil, err
	}
	return &v1.GetCheckerUploadResponse{UploadUrl: url, UploadExpireTime: timestamppb.New(time.Now().Add(uploadURLTTL))}, nil
}

// ConfirmChecker не доверяет подтверждению клиента без проверки S3.
func (s *Service) ConfirmChecker(ctx context.Context, req *v1.ConfirmCheckerRequest) (*v1.ConfirmCheckerResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, p, err := s.loadEditable(ctx, uuid.MustParse(req.GetProblemId()))
	if err != nil {
		return nil, err
	}
	if p.Checker != "custom" {
		return nil, status.Error(codes.FailedPrecondition, "задание не использует custom-чекер")
	}
	p.CheckerReady = true
	if err := s.checkChecker(ctx, p); err != nil {
		return nil, err
	}
	if err := s.repo.SetCheckerReady(ctx, p.ID, true); err != nil {
		return nil, err
	}
	return &v1.ConfirmCheckerResponse{}, nil
}

func (s *Service) checkChecker(ctx context.Context, p Problem) error {
	if !p.CheckerReady || p.CheckerKey == nil {
		return status.Error(codes.FailedPrecondition, "чекер не загружен и не подтверждён")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	n, err := s.objects.Size(ctx, *p.CheckerKey)
	if errors.Is(err, storage.ErrNotFound) || (err == nil && n == 0) {
		return status.Error(codes.FailedPrecondition, "чекер не загружен")
	}
	return err
}
