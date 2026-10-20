package submission

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/autocheck-team/backend/internal/auth"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

const defaultPageSize = 20

// maxSourceBytes повторяет ограничение из proto: защита, если сервис вызовут в обход валидации.
const maxSourceBytes = 64 << 10

// Objects — объектное хранилище для исходников.
type Objects interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}

// Service реализует SubmissionService.
type Service struct {
	v1.UnimplementedSubmissionServiceServer

	repo    *Repository
	objects Objects
}

// NewService создаёт Service.
func NewService(repo *Repository, objects Objects) *Service {
	return &Service{repo: repo, objects: objects}
}

// CreateSubmission принимает решение и ставит его в очередь проверки.
func (s *Service) CreateSubmission(ctx context.Context, req *v1.CreateSubmissionRequest) (*v1.CreateSubmissionResponse, error) {
	user, err := auth.Require(ctx)
	if err != nil {
		return nil, err
	}
	if len(req.GetSource()) > maxSourceBytes {
		return nil, status.Error(codes.InvalidArgument, "исходный код больше 64 КБ")
	}

	var requestID *uuid.UUID
	if req.GetClientRequestId() != "" {
		id := uuid.MustParse(req.GetClientRequestId())
		requestID = &id
		if existing, err := s.repo.FindByClientRequest(ctx, user.ID, id); err == nil {
			return &v1.CreateSubmissionResponse{Submission: toProto(existing)}, nil
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}

	problemID := uuid.MustParse(req.GetProblemId())
	target, err := s.repo.Target(ctx, problemID, req.GetLanguageId())
	if errors.Is(err, ErrNotFound) {
		return nil, status.Error(codes.NotFound, "задание не найдено")
	}
	if err != nil {
		return nil, err
	}
	canEdit := user.Has(auth.RoleAdmin) || target.AuthorID == user.ID
	if !target.IsPublished && !canEdit {
		return nil, status.Error(codes.NotFound, "задание не найдено")
	}
	if !target.LanguageAllowed {
		return nil, status.Errorf(codes.InvalidArgument, "язык %q недоступен для этого задания", req.GetLanguageId())
	}

	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("generate id: %w", err)
	}
	// Исходник сохраняется до записи в БД: воркер, забравший посылку из очереди,
	// всегда найдёт файл. Если запись в БД не удастся, останется лишний объект — это безопасно.
	key := fmt.Sprintf("submissions/%s/%s", id, target.SourceFile)
	if err := s.objects.Put(ctx, key, strings.NewReader(req.GetSource()), int64(len(req.GetSource())),
		"text/plain; charset=utf-8"); err != nil {
		return nil, err
	}

	created, err := s.repo.Create(ctx, Submission{
		ID: id, ProblemID: problemID, UserID: user.ID, LanguageID: req.GetLanguageId(),
		SourceKey: key, SourceSize: int32(len(req.GetSource())), //nolint:gosec // размер ограничен 64 КБ
		ClientRequestID: requestID,
	})
	if errors.Is(err, errDuplicateRequest) {
		// Параллельный повтор того же запроса успел создать посылку первым.
		created, err = s.repo.FindByClientRequest(ctx, user.ID, *requestID)
	}
	if err != nil {
		return nil, err
	}
	return &v1.CreateSubmissionResponse{Submission: toProto(created)}, nil
}

// GetSubmission возвращает посылку с результатами по тестам.
func (s *Service) GetSubmission(ctx context.Context, req *v1.GetSubmissionRequest) (*v1.GetSubmissionResponse, error) {
	sub, err := s.loadVisible(ctx, uuid.MustParse(req.GetId()))
	if err != nil {
		return nil, err
	}
	runs, err := s.repo.TestRuns(ctx, sub.ID)
	if err != nil {
		return nil, err
	}
	resp := &v1.GetSubmissionResponse{Submission: toProto(sub), TestRuns: make([]*v1.TestRun, 0, len(runs))}
	for _, r := range runs {
		resp.TestRuns = append(resp.TestRuns, &v1.TestRun{
			TestCaseId: r.TestCaseID.String(), Ordinal: r.Ordinal, IsSample: r.IsSample,
			Verdict: verdictToProto(&r.Verdict), TimeMs: r.TimeMS, MemoryKb: r.MemoryKB,
		})
	}
	return resp, nil
}

// ListSubmissions возвращает страницу посылок.
func (s *Service) ListSubmissions(ctx context.Context, req *v1.ListSubmissionsRequest) (*v1.ListSubmissionsResponse, error) {
	user, err := auth.Require(ctx)
	if err != nil {
		return nil, err
	}
	limit := int(req.GetPageSize())
	if limit == 0 {
		limit = defaultPageSize
	}
	f := ListFilter{UserID: user.ID, Limit: limit + 1}
	if req.GetProblemId() != "" {
		f.ProblemID = uuid.MustParse(req.GetProblemId())
		// Автор задания и администратор видят посылки всех пользователей по заданию.
		if user.Has(auth.RoleAdmin) {
			f.UserID = uuid.Nil
		} else if target, err := s.repo.Target(ctx, f.ProblemID, ""); err == nil && target.AuthorID == user.ID {
			f.UserID = uuid.Nil
		}
	}
	if req.GetPageToken() != "" {
		b, err := base64.RawURLEncoding.DecodeString(req.GetPageToken())
		if err == nil {
			f.After, err = uuid.FromBytes(b)
		}
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "некорректный page_token")
		}
	}

	subs, err := s.repo.List(ctx, f)
	if err != nil {
		return nil, err
	}
	resp := &v1.ListSubmissionsResponse{}
	if len(subs) > limit {
		subs = subs[:limit]
		last := subs[limit-1].ID
		resp.NextPageToken = base64.RawURLEncoding.EncodeToString(last[:])
	}
	resp.Submissions = make([]*v1.Submission, 0, len(subs))
	for _, sub := range subs {
		resp.Submissions = append(resp.Submissions, toProto(sub))
	}
	return resp, nil
}

// GetSubmissionSource возвращает исходный код посылки.
func (s *Service) GetSubmissionSource(
	ctx context.Context, req *v1.GetSubmissionSourceRequest,
) (*v1.GetSubmissionSourceResponse, error) {
	sub, err := s.loadVisible(ctx, uuid.MustParse(req.GetId()))
	if err != nil {
		return nil, err
	}
	rc, err := s.objects.Get(ctx, sub.SourceKey)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	src, err := io.ReadAll(io.LimitReader(rc, maxSourceBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read source %s: %w", sub.SourceKey, err)
	}
	return &v1.GetSubmissionSourceResponse{
		LanguageId: sub.LanguageID,
		FileName:   sub.SourceKey[strings.LastIndex(sub.SourceKey, "/")+1:],
		Source:     string(src),
	}, nil
}

// loadVisible загружает посылку, если пользователь может её видеть: свою, по своему
// заданию или любую для администратора. Чужая посылка выглядит как отсутствующая.
func (s *Service) loadVisible(ctx context.Context, id uuid.UUID) (Submission, error) {
	user, err := auth.Require(ctx)
	if err != nil {
		return Submission{}, err
	}
	sub, err := s.repo.Get(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return Submission{}, status.Error(codes.NotFound, "посылка не найдена")
	}
	if err != nil {
		return Submission{}, err
	}
	if sub.UserID != user.ID && sub.ProblemAuthorID != user.ID && !user.Has(auth.RoleAdmin) {
		return Submission{}, status.Error(codes.NotFound, "посылка не найдена")
	}
	return sub, nil
}

var statusToProto = map[string]v1.SubmissionStatus{
	"queued":    v1.SubmissionStatus_SUBMISSION_STATUS_QUEUED,
	"compiling": v1.SubmissionStatus_SUBMISSION_STATUS_COMPILING,
	"running":   v1.SubmissionStatus_SUBMISSION_STATUS_RUNNING,
	"done":      v1.SubmissionStatus_SUBMISSION_STATUS_DONE,
	"failed":    v1.SubmissionStatus_SUBMISSION_STATUS_FAILED,
}

var verdictsToProto = map[string]v1.Verdict{
	"OK": v1.Verdict_VERDICT_OK,
	"CE": v1.Verdict_VERDICT_COMPILATION_ERROR,
	"WA": v1.Verdict_VERDICT_WRONG_ANSWER,
	"TL": v1.Verdict_VERDICT_TIME_LIMIT_EXCEEDED,
	"ML": v1.Verdict_VERDICT_MEMORY_LIMIT_EXCEEDED,
	"RE": v1.Verdict_VERDICT_RUNTIME_ERROR,
	"SE": v1.Verdict_VERDICT_SYSTEM_ERROR,
}

func verdictToProto(v *string) v1.Verdict {
	if v == nil {
		return v1.Verdict_VERDICT_UNSPECIFIED
	}
	return verdictsToProto[*v]
}

func toProto(s Submission) *v1.Submission {
	out := &v1.Submission{
		Id:              s.ID.String(),
		ProblemId:       s.ProblemID.String(),
		UserId:          s.UserID.String(),
		LanguageId:      s.LanguageID,
		SourceSizeBytes: s.SourceSize,
		Status:          statusToProto[s.Status],
		Verdict:         verdictToProto(s.Verdict),
		Score:           s.Score,
		MaxTimeMs:       s.MaxTimeMS,
		MaxMemoryKb:     s.MaxMemoryKB,
		CreateTime:      timestamppb.New(s.CreatedAt),
	}
	if s.CompileLog != nil {
		out.CompileLog = *s.CompileLog
	}
	if s.StartedAt != nil {
		out.StartTime = timestamppb.New(*s.StartedAt)
	}
	if s.FinishedAt != nil {
		out.FinishTime = timestamppb.New(*s.FinishedAt)
	}
	return out
}
