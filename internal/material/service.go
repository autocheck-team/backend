package material

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/autocheck-team/backend/internal/auth"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

// Config задаёт предел размера и таймаут операций объектного хранилища.
type Config struct {
	MaxAttachmentBytes int64
	ObjectTimeout      time.Duration
}

// Service реализует сценарии учебных материалов и проверяет права на платные поля.
type Service struct {
	v1.UnimplementedMaterialServiceServer
	repo      Store
	billing   Entitlements
	objects   Objects
	validator protovalidate.Validator
	cfg       Config
}

// New собирает сценарии материалов с явными зависимостями и конфигурацией.
func New(repo Store, billing Entitlements, objects Objects, validator protovalidate.Validator, cfg Config) *Service {
	return &Service{repo: repo, billing: billing, objects: objects, validator: validator, cfg: cfg}
}

func (s *Service) authorize(ctx context.Context, request proto.Message, roles ...auth.Role) (auth.Principal, error) {
	user, err := auth.Require(ctx, roles...)
	if err != nil {
		return auth.Principal{}, err
	}
	if err := s.validator.Validate(request); err != nil {
		return auth.Principal{}, status.Error(codes.InvalidArgument, err.Error())
	}
	return user, nil
}

func inputMaterial(input *v1.MaterialInput) (Material, error) {
	if strings.TrimSpace(input.GetTitle()) == "" || strings.TrimSpace(input.GetContentMarkdown()) == "" {
		return Material{}, status.Error(codes.InvalidArgument, "заголовок и содержимое не могут состоять только из пробелов")
	}
	material := Material{
		Title: input.GetTitle(), Description: input.GetDescription(), ContentMarkdown: input.GetContentMarkdown(),
		Ordinal: input.GetOrdinal(), IsPublished: input.GetIsPublished(), ProblemIDs: make([]uuid.UUID, 0, len(input.GetProblemIds())),
	}
	for _, id := range input.GetProblemIds() {
		material.ProblemIDs = append(material.ProblemIDs, uuid.MustParse(id))
	}
	return material, nil
}

// CreateMaterial создаёт материал от платформы; повтор request_id возвращает исходную запись.
func (s *Service) CreateMaterial(ctx context.Context, req *v1.CreateMaterialRequest) (*v1.CreateMaterialResponse, error) {
	if _, err := s.authorize(ctx, req, auth.RoleAdmin); err != nil {
		return nil, err
	}
	material, err := inputMaterial(req.GetMaterial())
	if err != nil {
		return nil, err
	}
	material.RequestID = uuid.MustParse(req.GetRequestId())
	created, err := s.repo.Create(ctx, material)
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.CreateMaterialResponse{Material: toProto(created, false)}, nil
}

// UpdateMaterial заменяет поля материала, включая публикацию и привязки.
func (s *Service) UpdateMaterial(ctx context.Context, req *v1.UpdateMaterialRequest) (*v1.UpdateMaterialResponse, error) {
	if _, err := s.authorize(ctx, req, auth.RoleAdmin); err != nil {
		return nil, err
	}
	material, err := inputMaterial(req.GetMaterial())
	if err != nil {
		return nil, err
	}
	material.ID = uuid.MustParse(req.GetId())
	updated, err := s.repo.Update(ctx, material)
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.UpdateMaterialResponse{Material: toProto(updated, false)}, nil
}

// DeleteMaterial удаляет материал; повтор позволяет завершить очистку S3 после сбоя.
func (s *Service) DeleteMaterial(ctx context.Context, req *v1.DeleteMaterialRequest) (*v1.DeleteMaterialResponse, error) {
	if _, err := s.authorize(ctx, req, auth.RoleAdmin); err != nil {
		return nil, err
	}
	id := uuid.MustParse(req.GetId())
	if err := s.repo.Delete(ctx, id); err != nil && !errors.Is(err, ErrNotFound) {
		return nil, mapError(err)
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.ObjectTimeout)
	defer cancel()
	if err := s.objects.DeletePrefix(ctx, materialPrefix(id)); err != nil {
		return nil, fmt.Errorf("delete material objects: %w", err)
	}
	return &v1.DeleteMaterialResponse{}, nil
}

func (s *Service) accessible(ctx context.Context, user auth.Principal) (bool, error) {
	if user.Has(auth.RoleAdmin) {
		return true, nil
	}
	limits, err := s.billing.Limits(ctx, user.ID)
	if err != nil {
		return false, fmt.Errorf("read material entitlement: %w", err)
	}
	return limits.Plan.HasMaterialAccess, nil
}

// GetMaterial возвращает описание всем вошедшим и платные поля при действующем доступе.
func (s *Service) GetMaterial(ctx context.Context, req *v1.GetMaterialRequest) (*v1.GetMaterialResponse, error) {
	user, err := s.authorize(ctx, req)
	if err != nil {
		return nil, err
	}
	material, err := s.repo.Get(ctx, uuid.MustParse(req.GetId()))
	if err != nil {
		return nil, mapError(err)
	}
	if !material.IsPublished && !user.Has(auth.RoleAdmin) {
		return nil, status.Error(codes.NotFound, "материал не найден")
	}
	access, err := s.accessible(ctx, user)
	if err != nil {
		return nil, err
	}
	out := toProto(material, !access)
	if access {
		attachments, err := s.repo.Attachments(ctx, material.ID)
		if err != nil {
			return nil, mapError(err)
		}
		ctx, cancel := context.WithTimeout(ctx, s.cfg.ObjectTimeout)
		defer cancel()
		out.Attachments = make([]*v1.MaterialAttachment, 0, len(attachments))
		for _, attachment := range attachments {
			if !attachment.IsReady && !user.Has(auth.RoleAdmin) {
				continue
			}
			item := attachmentProto(attachment)
			if attachment.IsReady {
				item.DownloadUrl, err = s.objects.PresignGet(ctx, attachment.ObjectKey, downloadTTL)
				if err != nil {
					return nil, fmt.Errorf("sign material download: %w", err)
				}
				item.DownloadExpireTime = timestamppb.New(time.Now().Add(downloadTTL))
			}
			out.Attachments = append(out.Attachments, item)
		}
	}
	return &v1.GetMaterialResponse{Material: out}, nil
}

type pageCursor struct {
	ID        uuid.UUID `json:"id"`
	Ordinal   int32     `json:"ordinal"`
	ProblemID uuid.UUID `json:"problem_id"`
	All       bool      `json:"all"`
}

// ListMaterials возвращает описания; тот же метод обслуживает вложенный ресурс задания.
func (s *Service) ListMaterials(ctx context.Context, req *v1.ListMaterialsRequest) (*v1.ListMaterialsResponse, error) {
	user, err := s.authorize(ctx, req)
	if err != nil {
		return nil, err
	}
	limit := int(req.GetPageSize())
	if limit == 0 {
		limit = 20
	}
	filter := ListFilter{All: user.Has(auth.RoleAdmin), Limit: limit + 1}
	if req.GetProblemId() != "" {
		filter.ProblemID = uuid.MustParse(req.GetProblemId())
	}
	if req.GetPageToken() != "" {
		raw, decodeErr := base64.RawURLEncoding.DecodeString(req.GetPageToken())
		var cursor pageCursor
		if decodeErr != nil || json.Unmarshal(raw, &cursor) != nil || cursor.ID == uuid.Nil || cursor.Ordinal < 0 ||
			cursor.ProblemID != filter.ProblemID || cursor.All != filter.All {
			return nil, status.Error(codes.InvalidArgument, "некорректный page_token или изменён фильтр")
		}
		filter.After, filter.Ordinal = cursor.ID, cursor.Ordinal
	}
	materials, err := s.repo.List(ctx, filter)
	if err != nil {
		return nil, mapError(err)
	}
	access, err := s.accessible(ctx, user)
	if err != nil {
		return nil, err
	}
	response := &v1.ListMaterialsResponse{Materials: make([]*v1.Material, 0, min(limit, len(materials)))}
	if len(materials) > limit {
		materials = materials[:limit]
		last := materials[limit-1]
		raw, err := json.Marshal(pageCursor{ID: last.ID, Ordinal: last.Ordinal, ProblemID: filter.ProblemID, All: filter.All})
		if err != nil {
			return nil, fmt.Errorf("encode material cursor: %w", err)
		}
		response.NextPageToken = base64.RawURLEncoding.EncodeToString(raw)
	}
	for _, material := range materials {
		material.ContentMarkdown = ""
		response.Materials = append(response.Materials, toProto(material, !access))
	}
	return response, nil
}

func toProto(material Material, locked bool) *v1.Material {
	out := &v1.Material{
		Id: material.ID.String(), Title: material.Title, Description: material.Description,
		Ordinal: material.Ordinal, IsPublished: material.IsPublished, IsLocked: locked,
		ProblemIds: make([]string, 0, len(material.ProblemIDs)),
		CreateTime: timestamppb.New(material.CreatedAt), UpdateTime: timestamppb.New(material.UpdatedAt),
	}
	for _, id := range material.ProblemIDs {
		out.ProblemIds = append(out.ProblemIds, id.String())
	}
	if !locked {
		out.ContentMarkdown = material.ContentMarkdown
	}
	return out
}

func mapError(err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return status.Error(codes.NotFound, "материал или вложение не найдены")
	case errors.Is(err, ErrUnknownProblem):
		return status.Error(codes.InvalidArgument, "неизвестное задание в problem_ids")
	default:
		return err
	}
}
