package playground

import (
	"context"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/autocheck-team/backend/internal/auth"
	"github.com/autocheck-team/backend/internal/billing"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

// Service проверяет доступ к личному пространству и принимает снимки для воркера.
type Service struct {
	v1.UnimplementedPlaygroundServiceServer
	repo      Store
	validator protovalidate.Validator
}

// New создаёт сценарии playground без доступа к Docker или S3.
func New(repo Store, validator protovalidate.Validator) *Service {
	return &Service{repo: repo, validator: validator}
}

func validateFiles(files []File, sourceFile string) error {
	if len(files) == 0 || len(files) > maxFiles {
		return ErrFiles
	}
	names := make(map[string]bool, len(files))
	size := 0
	namePattern := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	found := sourceFile == ""
	for _, file := range files {
		if len(file.Name) > 100 || !namePattern.MatchString(file.Name) || names[file.Name] || !utf8.ValidString(file.Content) || strings.ContainsRune(file.Content, 0) {
			return ErrFiles
		}
		names[file.Name] = true
		size += len(file.Content)
		found = found || file.Name == sourceFile
	}
	if size > maxFileBytes || !found {
		return ErrFiles
	}
	return nil
}

func (s *Service) validate(message proto.Message) error {
	if err := s.validator.Validate(message); err != nil {
		return status.Error(codes.InvalidArgument, "invalid playground request")
	}
	return nil
}

func projectInput(userID uuid.UUID, title, languageID string, files []*v1.ProjectFile) (Project, error) {
	result := Project{UserID: userID, Title: title, LanguageID: languageID, Files: make([]File, 0, len(files))}
	for _, file := range files {
		result.Files = append(result.Files, File{Name: file.GetName(), Content: file.GetContent()})
	}
	if strings.TrimSpace(title) == "" || !utf8.ValidString(title) || strings.ContainsRune(title, 0) {
		return Project{}, status.Error(codes.InvalidArgument, "title is blank")
	}
	if err := validateFiles(result.Files, ""); err != nil {
		return Project{}, mapError(err)
	}
	return result, nil
}

// CreateProject принимает файлы и резервирует одно место в личном пространстве.
func (s *Service) CreateProject(ctx context.Context, req *v1.CreateProjectRequest) (*v1.CreateProjectResponse, error) {
	user, err := auth.Require(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.validate(req); err != nil {
		return nil, err
	}
	project, err := projectInput(user.ID, req.GetTitle(), req.GetLanguageId(), req.GetFiles())
	if err != nil {
		return nil, err
	}
	project.RequestID = uuid.MustParse(req.GetClientRequestId())
	created, err := s.repo.Create(ctx, project)
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.CreateProjectResponse{Project: projectProto(created)}, nil
}

// GetProject скрывает чужие проекты от всех, кроме администратора.
func (s *Service) GetProject(ctx context.Context, req *v1.GetProjectRequest) (*v1.GetProjectResponse, error) {
	user, err := auth.Require(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.validate(req); err != nil {
		return nil, err
	}
	project, err := s.repo.Get(ctx, uuid.MustParse(req.GetId()))
	if err != nil {
		return nil, mapError(err)
	}
	if project.UserID != user.ID && !user.Has(auth.RoleAdmin) {
		return nil, mapError(ErrNotFound)
	}
	return &v1.GetProjectResponse{Project: projectProto(project)}, nil
}

// ListProjects применяет владельца до keyset-пагинации, в том числе для администратора.
func (s *Service) ListProjects(ctx context.Context, req *v1.ListProjectsRequest) (*v1.ListProjectsResponse, error) {
	user, err := auth.Require(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.validate(req); err != nil {
		return nil, err
	}
	limit := int(req.GetPageSize())
	if limit == 0 {
		limit = 20
	}
	after := uuid.Nil
	if req.GetPageToken() != "" {
		data, err := base64.RawURLEncoding.DecodeString(req.GetPageToken())
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "invalid page_token")
		}
		after, err = uuid.FromBytes(data)
		if err != nil || after == uuid.Nil {
			return nil, status.Error(codes.InvalidArgument, "invalid page_token")
		}
	}
	projects, err := s.repo.List(ctx, user.ID, after, limit+1)
	if err != nil {
		return nil, mapError(err)
	}
	response := &v1.ListProjectsResponse{Projects: make([]*v1.Project, 0, min(limit, len(projects)))}
	if len(projects) > limit {
		projects = projects[:limit]
		last := projects[limit-1].ID
		response.NextPageToken = base64.RawURLEncoding.EncodeToString(last[:])
	}
	for _, project := range projects {
		response.Projects = append(response.Projects, projectProto(project))
	}
	return response, nil
}

// UpdateProject заменяет редактируемые поля только у владельца.
func (s *Service) UpdateProject(ctx context.Context, req *v1.UpdateProjectRequest) (*v1.UpdateProjectResponse, error) {
	user, err := auth.Require(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.validate(req); err != nil {
		return nil, err
	}
	project, err := projectInput(user.ID, req.GetTitle(), req.GetLanguageId(), req.GetFiles())
	if err != nil {
		return nil, err
	}
	project.ID = uuid.MustParse(req.GetId())
	updated, err := s.repo.Update(ctx, project)
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.UpdateProjectResponse{Project: projectProto(updated)}, nil
}

// DeleteProject удаляет только свой проект, не отменяя принятые запуски.
func (s *Service) DeleteProject(ctx context.Context, req *v1.DeleteProjectRequest) (*v1.DeleteProjectResponse, error) {
	user, err := auth.Require(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.validate(req); err != nil {
		return nil, err
	}
	if err := s.repo.Delete(ctx, uuid.MustParse(req.GetId()), user.ID); err != nil {
		return nil, mapError(err)
	}
	return &v1.DeleteProjectResponse{}, nil
}

// RunProject принимает снимок и расходует квоту один раз по ключу идемпотентности.
func (s *Service) RunProject(ctx context.Context, req *v1.RunProjectRequest) (*v1.RunProjectResponse, error) {
	user, err := auth.Require(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.validate(req); err != nil {
		return nil, err
	}
	if !utf8.ValidString(req.GetStdin()) || strings.ContainsRune(req.GetStdin(), 0) {
		return nil, status.Error(codes.InvalidArgument, "stdin must be UTF-8 without NUL")
	}
	run, err := s.repo.Enqueue(ctx, uuid.MustParse(req.GetId()), user.ID, uuid.MustParse(req.GetClientRequestId()), req.GetStdin())
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.RunProjectResponse{Run: runProto(run)}, nil
}

// GetProjectRun разрешает чтение владельцу и администратору, включая удалённые проекты.
func (s *Service) GetProjectRun(ctx context.Context, req *v1.GetProjectRunRequest) (*v1.GetProjectRunResponse, error) {
	user, err := auth.Require(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.validate(req); err != nil {
		return nil, err
	}
	run, err := s.repo.GetRun(ctx, uuid.MustParse(req.GetId()))
	if err != nil {
		return nil, mapError(err)
	}
	if run.UserID != user.ID && !user.Has(auth.RoleAdmin) {
		return nil, mapError(ErrNotFound)
	}
	return &v1.GetProjectRunResponse{Run: runProto(run)}, nil
}

func mapError(err error) error {
	var limit *billing.LimitError
	switch {
	case errors.Is(err, ErrNotFound):
		return status.Error(codes.NotFound, "project or run not found")
	case errors.Is(err, ErrProjectLimit):
		return status.Error(codes.ResourceExhausted, "project limit is 20")
	case errors.Is(err, ErrLanguage):
		return status.Error(codes.InvalidArgument, "language is not active")
	case errors.Is(err, ErrFiles):
		return status.Error(codes.InvalidArgument, "invalid files or missing language source_file")
	case errors.Is(err, ErrRequestConflict):
		return status.Error(codes.AlreadyExists, "request id belongs to another project")
	case errors.As(err, &limit):
		result, detailErr := status.New(codes.ResourceExhausted, "compilation limit exhausted").WithDetails(
			&errdetails.RetryInfo{RetryDelay: durationpb.New(limit.RetryAfter)})
		if detailErr != nil {
			return status.Error(codes.ResourceExhausted, "compilation limit exhausted")
		}
		return result.Err()
	default:
		return err
	}
}
