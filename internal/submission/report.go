package submission

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

const reportTextBytes = 4 << 10

// GetSubmissionReport использует ту же проверку видимости, что и получение посылки.
func (s *Service) GetSubmissionReport(ctx context.Context,
	req *v1.GetSubmissionReportRequest,
) (*v1.GetSubmissionReportResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	sub, err := s.loadVisible(ctx, uuid.MustParse(req.GetId()))
	if err != nil {
		return nil, err
	}
	runs, err := s.repo.TestRuns(ctx, sub.ID)
	if err != nil {
		return nil, err
	}
	return s.buildReport(ctx, sub, runs)
}

func (s *Service) buildReport(ctx context.Context, sub Submission, runs []TestRun) (*v1.GetSubmissionReportResponse, error) {
	resp := &v1.GetSubmissionReportResponse{
		LanguageId: sub.LanguageID, Status: statusToProto[sub.Status],
		Verdict: verdictToProto(sub.Verdict), Score: sub.Score,
		TestRuns: make([]*v1.SubmissionTestReport, 0, len(runs)),
	}
	if sub.Verdict != nil && *sub.Verdict == "CE" && sub.CompileLog != nil {
		resp.CompileLog = *sub.CompileLog
	}
	for _, run := range runs {
		test := &v1.SubmissionTestReport{
			Ordinal: run.Ordinal, IsSample: run.IsSample, Verdict: verdictToProto(&run.Verdict),
			TimeMs: run.TimeMS, MemoryKb: run.MemoryKB, CpuTimeMs: run.CPUTimeMS,
			IsTimeLimitExceeded: run.Verdict == "TL", IsMemoryLimitExceeded: run.Verdict == "ML",
		}
		// Даже автор и администратор не получают закрытые данные через отчёт.
		if run.IsSample && run.Verdict == "WA" {
			if err := s.addReportDetails(ctx, test, run); err != nil {
				return nil, err
			}
		}
		resp.TestRuns = append(resp.TestRuns, test)
	}
	return resp, nil
}

func (s *Service) addReportDetails(ctx context.Context, test *v1.SubmissionTestReport, run TestRun) error {
	var err error
	if test.Input, err = s.reportText(ctx, run.InputKey); err != nil {
		return err
	}
	if test.ExpectedOutput, err = s.reportText(ctx, run.ExpectedKey); err != nil {
		return err
	}
	// Старые прогоны могли не сохранить stdout; отсутствие отличается от пустого файла.
	if run.StdoutKey == nil {
		return nil
	}
	if test.ActualOutput, err = s.reportText(ctx, *run.StdoutKey); err != nil {
		return err
	}
	test.Diff = lineDiff(test.ExpectedOutput.GetText(), test.ActualOutput.GetText())
	return nil
}

func (s *Service) reportText(ctx context.Context, key string) (text *v1.ReportText, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rc, err := s.objects.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("get report object: %w", err)
	}
	defer func() {
		if closeErr := rc.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close report object: %w", closeErr))
		}
	}()
	data, err := io.ReadAll(io.LimitReader(rc, reportTextBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read report object: %w", err)
	}
	truncated := len(data) > reportTextBytes
	data = data[:min(len(data), reportTextBytes)]
	value := strings.ToValidUTF8(string(data), "\uFFFD")
	if len(value) > reportTextBytes {
		value = value[:reportTextBytes]
		truncated = true
	}
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return &v1.ReportText{Text: value, IsTruncated: truncated}, nil
}
