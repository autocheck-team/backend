package problem

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

var checkerToDB = map[v1.Checker]string{
	v1.Checker_CHECKER_EXACT:  "exact",
	v1.Checker_CHECKER_TOKENS: "tokens",
	v1.Checker_CHECKER_FLOAT:  "float",
	v1.Checker_CHECKER_CUSTOM: "custom",
}

var checkerFromDB = map[string]v1.Checker{
	"exact":  v1.Checker_CHECKER_EXACT,
	"tokens": v1.Checker_CHECKER_TOKENS,
	"float":  v1.Checker_CHECKER_FLOAT,
	"custom": v1.Checker_CHECKER_CUSTOM,
}

// fromProto переводит задание из API в модель. Поля, которые задаёт сервер
// (id, автор, время, число тестов), не переносятся.
func fromProto(p *v1.Problem) (Problem, error) {
	checker, ok := checkerToDB[p.GetChecker()]
	if !ok {
		return Problem{}, status.Error(codes.InvalidArgument, "неизвестный checker")
	}
	if p.GetChecker() == v1.Checker_CHECKER_CUSTOM {
		return Problem{}, status.Error(codes.Unimplemented, "собственные чекеры пока не поддерживаются")
	}
	out := Problem{
		Slug:          p.GetSlug(),
		Title:         p.GetTitle(),
		Statement:     p.GetStatement(),
		TimeLimitMS:   p.GetTimeLimitMs(),
		MemoryLimitKB: p.GetMemoryLimitKb(),
		Checker:       checker,
		LanguageIDs:   p.GetLanguageIds(),
		AllowAIHints:  p.GetAllowAiHints(),
		IsPublished:   p.GetIsPublished(),
	}
	// Погрешность имеет смысл только для CHECKER_FLOAT: для остальных не храним.
	if p.GetChecker() == v1.Checker_CHECKER_FLOAT && p.FloatEpsilon != nil {
		eps := p.GetFloatEpsilon()
		out.FloatEpsilon = &eps
	}
	return out, nil
}

func toProto(p Problem) *v1.Problem {
	return &v1.Problem{
		Id:             p.ID.String(),
		AuthorId:       p.AuthorID.String(),
		Slug:           p.Slug,
		Title:          p.Title,
		Statement:      p.Statement,
		TimeLimitMs:    p.TimeLimitMS,
		MemoryLimitKb:  p.MemoryLimitKB,
		Checker:        checkerFromDB[p.Checker],
		FloatEpsilon:   p.FloatEpsilon,
		LanguageIds:    p.LanguageIDs,
		AllowAiHints:   p.AllowAIHints,
		IsPublished:    p.IsPublished,
		ReadyTestCount: p.ReadyTestCount,
		CreateTime:     timestamppb.New(p.CreatedAt),
		UpdateTime:     timestamppb.New(p.UpdatedAt),
	}
}

// applyMask копирует в dst поля из src, перечисленные в paths. Изменять можно
// только поля из белого списка: id, автор и служебные поля задаёт сервер.
func applyMask(dst, src *v1.Problem, paths []string) error {
	if len(paths) == 0 {
		return status.Error(codes.InvalidArgument, "update_mask пуст: укажите, какие поля менять")
	}
	for _, path := range paths {
		switch path {
		case "slug":
			dst.Slug = src.GetSlug()
		case "title":
			dst.Title = src.GetTitle()
		case "statement":
			dst.Statement = src.GetStatement()
		case "time_limit_ms":
			dst.TimeLimitMs = src.GetTimeLimitMs()
		case "memory_limit_kb":
			dst.MemoryLimitKb = src.GetMemoryLimitKb()
		case "checker":
			dst.Checker = src.GetChecker()
		case "float_epsilon":
			dst.FloatEpsilon = src.FloatEpsilon
		case "language_ids":
			dst.LanguageIds = src.GetLanguageIds()
		case "allow_ai_hints":
			dst.AllowAiHints = src.GetAllowAiHints()
		case "is_published":
			dst.IsPublished = src.GetIsPublished()
		default:
			return status.Errorf(codes.InvalidArgument, "поле %q нельзя менять", path)
		}
	}
	return nil
}

func testCaseToProto(tc TestCase) *v1.TestCase {
	return &v1.TestCase{
		Id:                tc.ID.String(),
		ProblemId:         tc.ProblemID.String(),
		Ordinal:           tc.Ordinal,
		IsSample:          tc.IsSample,
		Points:            tc.Points,
		IsReady:           tc.IsReady,
		InputSizeBytes:    tc.InputSize,
		ExpectedSizeBytes: tc.ExpectedSize,
	}
}
