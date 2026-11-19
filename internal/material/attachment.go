package material

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/autocheck-team/backend/internal/auth"
	"github.com/autocheck-team/backend/internal/storage"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

const (
	uploadTTL   = 15 * time.Minute
	downloadTTL = 5 * time.Minute
)

func materialPrefix(id uuid.UUID) string { return "materials/" + id.String() + "/" }
func attachmentPrefix(materialID, id uuid.UUID) string {
	return materialPrefix(materialID) + "attachments/" + id.String() + "/"
}

func attachmentProto(attachment Attachment) *v1.MaterialAttachment {
	return &v1.MaterialAttachment{
		Id: attachment.ID.String(), Filename: attachment.Filename,
		ContentType: attachment.ContentType, SizeBytes: attachment.SizeBytes, IsReady: attachment.IsReady,
	}
}

// CreateMaterialAttachment выдаёт ссылку только admin; размер проверяется при подтверждении.
func (s *Service) CreateMaterialAttachment(ctx context.Context, req *v1.CreateMaterialAttachmentRequest) (*v1.CreateMaterialAttachmentResponse, error) {
	if _, err := s.authorize(ctx, req, auth.RoleAdmin); err != nil {
		return nil, err
	}
	name := req.GetFilename()
	if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "/\\") || strings.ContainsFunc(name, unicode.IsControl) || name == "." || name == ".." {
		return nil, status.Error(codes.InvalidArgument, "filename должен быть именем файла без пути")
	}
	materialID := uuid.MustParse(req.GetMaterialId())
	attachment, err := s.repo.CreateAttachment(ctx, Attachment{MaterialID: materialID, RequestID: uuid.MustParse(req.GetRequestId()), Filename: name})
	if err != nil {
		return nil, mapError(err)
	}
	response := &v1.CreateMaterialAttachmentResponse{Attachment: attachmentProto(attachment), MaxSizeBytes: s.cfg.MaxAttachmentBytes}
	if !attachment.IsReady {
		ctx, cancel := context.WithTimeout(ctx, s.cfg.ObjectTimeout)
		defer cancel()
		response.UploadUrl, err = s.objects.PresignPut(ctx, attachmentPrefix(materialID, attachment.ID)+"upload", uploadTTL)
		if err != nil {
			return nil, fmt.Errorf("sign material upload: %w", err)
		}
		response.UploadExpireTime = timestamppb.New(time.Now().Add(uploadTTL))
	}
	return response, nil
}

func (s *Service) readUpload(ctx context.Context, key string) ([]byte, error) {
	reader, err := s.objects.Get(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, status.Error(codes.FailedPrecondition, "вложение не загружено")
	}
	if err != nil {
		return nil, fmt.Errorf("open material upload: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, s.cfg.MaxAttachmentBytes+1))
	closeErr := reader.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, fmt.Errorf("read material upload: %w", err)
	}
	if len(data) == 0 || int64(len(data)) > s.cfg.MaxAttachmentBytes {
		return nil, status.Errorf(codes.InvalidArgument, "размер вложения должен быть от 1 до %d байт", s.cfg.MaxAttachmentBytes)
	}
	return data, nil
}

// ConfirmMaterialAttachment фиксирует проверенную копию, которую нельзя перезаписать PUT-ссылкой.
func (s *Service) ConfirmMaterialAttachment(ctx context.Context, req *v1.ConfirmMaterialAttachmentRequest) (*v1.ConfirmMaterialAttachmentResponse, error) {
	if _, err := s.authorize(ctx, req, auth.RoleAdmin); err != nil {
		return nil, err
	}
	attachment, err := s.repo.Attachment(ctx, uuid.MustParse(req.GetMaterialId()), uuid.MustParse(req.GetId()))
	if err != nil {
		return nil, mapError(err)
	}
	if attachment.IsReady {
		return &v1.ConfirmMaterialAttachmentResponse{Attachment: attachmentProto(attachment)}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.ObjectTimeout)
	defer cancel()
	prefix := attachmentPrefix(attachment.MaterialID, attachment.ID)
	data, err := s.readUpload(ctx, prefix+"upload")
	if err != nil {
		return nil, err
	}
	contentType := http.DetectContentType(data)
	switch contentType {
	case "application/pdf", "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return nil, status.Error(codes.InvalidArgument, "допустимы PDF, PNG, JPEG, GIF и WebP")
	}
	// У каждой попытки свой закрытый ключ: параллельный confirm не подменит выбранную БД копию.
	attachment.ObjectKey = prefix + "confirmed/" + uuid.NewString()
	attachment.ContentType, attachment.SizeBytes = contentType, int64(len(data))
	if err := s.objects.Put(ctx, attachment.ObjectKey, bytes.NewReader(data), int64(len(data)), contentType); err != nil {
		return nil, fmt.Errorf("freeze material attachment: %w", err)
	}
	confirmed, err := s.repo.ConfirmAttachment(ctx, attachment)
	if err != nil || confirmed.ObjectKey != attachment.ObjectKey {
		cleanupErr := s.objects.DeletePrefix(ctx, attachment.ObjectKey)
		if cleanupErr != nil {
			return nil, errors.Join(mapError(err), fmt.Errorf("clean unused material copy: %w", cleanupErr))
		}
	}
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.ConfirmMaterialAttachmentResponse{Attachment: attachmentProto(confirmed)}, nil
}

// DeleteMaterialAttachment удаляет метаданные и файлы; повтор безопасен после частичного сбоя.
func (s *Service) DeleteMaterialAttachment(ctx context.Context, req *v1.DeleteMaterialAttachmentRequest) (*v1.DeleteMaterialAttachmentResponse, error) {
	if _, err := s.authorize(ctx, req, auth.RoleAdmin); err != nil {
		return nil, err
	}
	materialID, id := uuid.MustParse(req.GetMaterialId()), uuid.MustParse(req.GetId())
	if err := s.repo.DeleteAttachment(ctx, materialID, id); err != nil && !errors.Is(err, ErrNotFound) {
		return nil, mapError(err)
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.ObjectTimeout)
	defer cancel()
	if err := s.objects.DeletePrefix(ctx, attachmentPrefix(materialID, id)); err != nil {
		return nil, fmt.Errorf("delete attachment objects: %w", err)
	}
	return &v1.DeleteMaterialAttachmentResponse{}, nil
}
