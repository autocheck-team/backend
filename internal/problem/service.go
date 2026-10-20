package problem

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/autocheck-team/backend/internal/auth"
	"github.com/autocheck-team/backend/internal/storage"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

const (
	defaultPageSize = 20
	uploadURLTTL    = 15 * time.Minute
	downloadURLTTL  = 15 * time.Minute
)

// Objects — объектное хранилище для файлов тестов.
type Objects interface {
	PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error)
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	Size(ctx context.Context, key string) (int64, error)
	DeletePrefix(ctx context.Context, prefix string) error
}

// Service реализует ProblemService.
type Service struct {
	v1.UnimplementedProblemServiceServer

	repo      *Repository
	objects   Objects
	validator protovalidate.Validator
}

// NewService создаёт Service.
func NewService(repo *Repository, objects Objects, validator protovalidate.Validator) *Service {
	return &Service{repo: repo, objects: objects, validator: validator}
}

// ListLanguages возвращает доступные языки.
func (s *Service) ListLanguages(ctx context.Context, _ *v1.ListLanguagesRequest) (*v1.ListLanguagesResponse, error) {
	langs, err := s.repo.ListLanguages(ctx)
	if err != nil {
		return nil, err
	}
	resp := &v1.ListLanguagesResponse{Languages: make([]*v1.Language, 0, len(langs))}
	for _, l := range langs {
		resp.Languages = append(resp.Languages, &v1.Language{Id: l.ID, Name: l.Name})
	}
	return resp, nil
}

// CreateProblem создаёт задание.
func (s *Service) CreateProblem(ctx context.Context, req *v1.CreateProblemRequest) (*v1.CreateProblemResponse, error) {
	user, err := auth.Require(ctx, auth.RoleAuthor, auth.RoleAdmin)
	if err != nil {
		return nil, err
	}
	p, err := fromProto(req.GetProblem())
	if err != nil {
		return nil, err
	}
	if p.IsPublished {
		return nil, status.Error(codes.FailedPrecondition, "задание без тестов нельзя опубликовать: сначала добавьте тесты")
	}
	p.ID, err = uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("generate id: %w", err)
	}
	p.AuthorID = user.ID

	created, err := s.repo.Create(ctx, p)
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.CreateProblemResponse{Problem: toProto(created)}, nil
}

// GetProblem возвращает задание.
func (s *Service) GetProblem(ctx context.Context, req *v1.GetProblemRequest) (*v1.GetProblemResponse, error) {
	user, err := auth.Require(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.repo.Get(ctx, uuid.MustParse(req.GetId()))
	if err != nil {
		return nil, mapError(err)
	}
	if !canView(user, p) {
		return nil, status.Error(codes.NotFound, "задание не найдено")
	}
	return &v1.GetProblemResponse{Problem: toProto(p)}, nil
}

// ListProblems возвращает страницу заданий.
func (s *Service) ListProblems(ctx context.Context, req *v1.ListProblemsRequest) (*v1.ListProblemsResponse, error) {
	user, err := auth.Require(ctx)
	if err != nil {
		return nil, err
	}
	limit := int(req.GetPageSize())
	if limit == 0 {
		limit = defaultPageSize
	}
	f := ListFilter{All: user.Has(auth.RoleAdmin), VisibleTo: user.ID, Limit: limit + 1}
	if req.GetOnlyMine() {
		f.OnlyAuthor = user.ID
	}
	if req.GetPageToken() != "" {
		if f.After, err = decodePageToken(req.GetPageToken()); err != nil {
			return nil, status.Error(codes.InvalidArgument, "некорректный page_token")
		}
	}

	problems, err := s.repo.List(ctx, f)
	if err != nil {
		return nil, err
	}
	resp := &v1.ListProblemsResponse{}
	// Запрашиваем на одно задание больше, чтобы понять, есть ли следующая страница.
	if len(problems) > limit {
		problems = problems[:limit]
		resp.NextPageToken = encodePageToken(problems[limit-1].ID)
	}
	resp.Problems = make([]*v1.Problem, 0, len(problems))
	for _, p := range problems {
		resp.Problems = append(resp.Problems, toProto(p))
	}
	return resp, nil
}

// UpdateProblem меняет поля из update_mask.
func (s *Service) UpdateProblem(ctx context.Context, req *v1.UpdateProblemRequest) (*v1.UpdateProblemResponse, error) {
	if req.GetProblem() == nil {
		return nil, status.Error(codes.InvalidArgument, "problem обязателен")
	}
	id, err := uuid.Parse(req.GetProblem().GetId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "problem.id должен быть UUID")
	}
	_, current, err := s.loadEditable(ctx, id)
	if err != nil {
		return nil, err
	}

	merged := toProto(current)
	if err := applyMask(merged, req.GetProblem(), req.GetUpdateMask().GetPaths()); err != nil {
		return nil, err
	}
	// Правила Problem проверяются для итогового задания: в запросе заполнены только
	// изменяемые поля, поэтому интерсептор валидации их не проверяет.
	if err := s.validator.Validate(merged); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	p, err := fromProto(merged)
	if err != nil {
		return nil, err
	}
	p.ID, p.AuthorID = current.ID, current.AuthorID
	if p.IsPublished && !current.IsPublished && current.ReadyTestCount == 0 {
		return nil, status.Error(codes.FailedPrecondition, "задание без загруженных тестов нельзя опубликовать")
	}
	updated, err := s.repo.Update(ctx, p)
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.UpdateProblemResponse{Problem: toProto(updated)}, nil
}

// DeleteProblem удаляет задание и файлы его тестов.
func (s *Service) DeleteProblem(ctx context.Context, req *v1.DeleteProblemRequest) (*v1.DeleteProblemResponse, error) {
	id := uuid.MustParse(req.GetId())
	if _, _, err := s.loadEditable(ctx, id); err != nil {
		return nil, err
	}
	if err := s.repo.Delete(ctx, id); err != nil {
		return nil, mapError(err)
	}
	// Файлы удаляются после фиксации в БД: если хранилище недоступно, останутся
	// лишние объекты, но не тесты без файлов.
	if err := s.objects.DeletePrefix(ctx, problemPrefix(id)); err != nil {
		return nil, fmt.Errorf("delete problem files: %w", err)
	}
	return &v1.DeleteProblemResponse{}, nil
}

// CreateTestCase создаёт тест и выдаёт ссылки для загрузки файлов.
func (s *Service) CreateTestCase(ctx context.Context, req *v1.CreateTestCaseRequest) (*v1.CreateTestCaseResponse, error) {
	problemID := uuid.MustParse(req.GetProblemId())
	if _, _, err := s.loadEditable(ctx, problemID); err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("generate id: %w", err)
	}
	points := int32(1)
	if req.Points != nil {
		points = req.GetPoints()
	}
	prefix := fmt.Sprintf("%s/tests/%s", problemPrefix(problemID), id)
	tc, err := s.repo.CreateTestCase(ctx, TestCase{
		ID: id, ProblemID: problemID, IsSample: req.GetIsSample(), Points: points,
		InputKey: prefix + "/input", ExpectedKey: prefix + "/expected",
	})
	if err != nil {
		return nil, mapError(err)
	}

	inURL, err := s.objects.PresignPut(ctx, tc.InputKey, uploadURLTTL)
	if err != nil {
		return nil, err
	}
	expURL, err := s.objects.PresignPut(ctx, tc.ExpectedKey, uploadURLTTL)
	if err != nil {
		return nil, err
	}
	return &v1.CreateTestCaseResponse{
		TestCase:          testCaseToProto(tc),
		InputUploadUrl:    inURL,
		ExpectedUploadUrl: expURL,
		UploadExpireTime:  timestamppb.New(time.Now().Add(uploadURLTTL)),
	}, nil
}

// ConfirmTestCase проверяет загрузку файлов и включает тест.
func (s *Service) ConfirmTestCase(ctx context.Context, req *v1.ConfirmTestCaseRequest) (*v1.ConfirmTestCaseResponse, error) {
	problemID := uuid.MustParse(req.GetProblemId())
	if _, _, err := s.loadEditable(ctx, problemID); err != nil {
		return nil, err
	}
	tc, err := s.repo.GetTestCase(ctx, problemID, uuid.MustParse(req.GetId()))
	if err != nil {
		return nil, mapError(err)
	}
	inSize, err := s.objects.Size(ctx, tc.InputKey)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, status.Error(codes.FailedPrecondition, "входные данные теста не загружены")
	}
	if err != nil {
		return nil, err
	}
	expSize, err := s.objects.Size(ctx, tc.ExpectedKey)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, status.Error(codes.FailedPrecondition, "эталонный вывод теста не загружен")
	}
	if err != nil {
		return nil, err
	}
	if err := s.repo.MarkTestCaseReady(ctx, tc.ID, inSize, expSize); err != nil {
		return nil, err
	}
	tc.IsReady, tc.InputSize, tc.ExpectedSize = true, inSize, expSize
	return &v1.ConfirmTestCaseResponse{TestCase: testCaseToProto(tc)}, nil
}

// ListTestCases возвращает тесты со ссылками на скачивание.
func (s *Service) ListTestCases(ctx context.Context, req *v1.ListTestCasesRequest) (*v1.ListTestCasesResponse, error) {
	user, err := auth.Require(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.repo.Get(ctx, uuid.MustParse(req.GetProblemId()))
	if err != nil {
		return nil, mapError(err)
	}
	if !canView(user, p) {
		return nil, status.Error(codes.NotFound, "задание не найдено")
	}

	tcs, err := s.repo.ListTestCases(ctx, p.ID, !canEdit(user, p))
	if err != nil {
		return nil, err
	}
	resp := &v1.ListTestCasesResponse{TestCases: make([]*v1.TestCase, 0, len(tcs))}
	for _, tc := range tcs {
		out := testCaseToProto(tc)
		if tc.IsReady {
			if out.InputDownloadUrl, err = s.objects.PresignGet(ctx, tc.InputKey, downloadURLTTL); err != nil {
				return nil, err
			}
			if out.ExpectedDownloadUrl, err = s.objects.PresignGet(ctx, tc.ExpectedKey, downloadURLTTL); err != nil {
				return nil, err
			}
		}
		resp.TestCases = append(resp.TestCases, out)
	}
	return resp, nil
}

// DeleteTestCase удаляет тест и его файлы.
func (s *Service) DeleteTestCase(ctx context.Context, req *v1.DeleteTestCaseRequest) (*v1.DeleteTestCaseResponse, error) {
	problemID, id := uuid.MustParse(req.GetProblemId()), uuid.MustParse(req.GetId())
	if _, _, err := s.loadEditable(ctx, problemID); err != nil {
		return nil, err
	}
	if err := s.repo.DeleteTestCase(ctx, problemID, id); err != nil {
		return nil, mapError(err)
	}
	if err := s.objects.DeletePrefix(ctx, fmt.Sprintf("%s/tests/%s/", problemPrefix(problemID), id)); err != nil {
		return nil, fmt.Errorf("delete test case files: %w", err)
	}
	return &v1.DeleteTestCaseResponse{}, nil
}

// loadEditable загружает задание и проверяет, что пользователь может его менять.
func (s *Service) loadEditable(ctx context.Context, id uuid.UUID) (auth.Principal, Problem, error) {
	user, err := auth.Require(ctx, auth.RoleAuthor, auth.RoleAdmin)
	if err != nil {
		return auth.Principal{}, Problem{}, err
	}
	p, err := s.repo.Get(ctx, id)
	if err != nil {
		return auth.Principal{}, Problem{}, mapError(err)
	}
	if !canEdit(user, p) {
		// Чужое неопубликованное задание выглядит как отсутствующее.
		if !canView(user, p) {
			return auth.Principal{}, Problem{}, status.Error(codes.NotFound, "задание не найдено")
		}
		return auth.Principal{}, Problem{}, status.Error(codes.PermissionDenied, "менять задание может только его автор")
	}
	return user, p, nil
}

func canView(u auth.Principal, p Problem) bool {
	return p.IsPublished || canEdit(u, p)
}

func canEdit(u auth.Principal, p Problem) bool {
	return u.Has(auth.RoleAdmin) || (u.Has(auth.RoleAuthor) && p.AuthorID == u.ID)
}

func problemPrefix(id uuid.UUID) string {
	return "problems/" + id.String()
}

func encodePageToken(last uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString(last[:])
}

func decodePageToken(token string) (uuid.UUID, error) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return uuid.Nil, err
	}
	return uuid.FromBytes(b)
}

// mapError переводит ошибки репозитория в gRPC-статусы. Остальное уходит как есть
// и превращается в Internal интерсептором.
func mapError(err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return status.Error(codes.NotFound, "не найдено")
	case errors.Is(err, ErrSlugTaken):
		return status.Error(codes.AlreadyExists, "задание с таким slug уже есть")
	case errors.Is(err, ErrUnknownLanguage):
		return status.Error(codes.InvalidArgument, "неизвестный или выключенный язык в language_ids")
	case errors.Is(err, ErrHasSubmissions):
		return status.Error(codes.FailedPrecondition, "по заданию уже есть посылки: снимите его с публикации вместо удаления")
	default:
		return err
	}
}
