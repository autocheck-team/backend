package submission

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

func TestLineDiff(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, expected, actual, diff string }{
		{"empty", "", "", ""},
		{"equal", "a\nb\n", "a\nb\n", " a\n b\n"},
		{"replace", "a\nb\n", "a\nc\n", " a\n-b\n+c\n"},
		{"insert", "a\nc\n", "a\nb\nc\n", " a\n+b\n c\n"},
		{"delete", "a\nb\nc\n", "a\nc\n", " a\n-b\n c\n"},
		{"add_all", "", "a\n", "+a\n"},
		{"delete_all", "a\n", "", "-a\n"},
		{"newline", "a\n", "a", "-a\n+a\n\\ No newline at end of file\n"},
		{"crlf", "a\r\n", "a\n", "-a\r\n+a\n"},
		{"blank_line", "\n", "", "-\n"},
		{"unicode", "да\n", "нет\n", "-да\n+нет\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := lineDiff(tc.expected, tc.actual); got != tc.diff {
				t.Fatalf("diff = %q, want %q", got, tc.diff)
			}
		})
	}
}

func TestBuildReport(t *testing.T) {
	t.Parallel()
	stdout := "out"
	for _, tc := range []struct {
		name                    string
		sample                  bool
		verdict                 string
		stdout                  *string
		details, actual, tl, ml bool
	}{
		{"open_wa", true, "WA", &stdout, true, true, false, false},
		{"closed_wa", false, "WA", &stdout, false, false, false, false},
		{"open_ok", true, "OK", &stdout, false, false, false, false},
		{"open_tl", true, "TL", &stdout, false, false, true, false},
		{"closed_ml", false, "ML", nil, false, false, false, true},
		{"legacy_without_stdout", true, "WA", nil, true, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// arrange
			objects := &memObjects{data: map[string][]byte{"in": []byte("2 3\n"), "exp": []byte("5\n"), "out": []byte("6\n")}}
			if !tc.details {
				objects.data = nil // Любая попытка чтения закрытых данных завершит тест ошибкой.
			}
			svc := NewService(nil, objects, nil)
			verdict, compileLog, score := "WA", "compiler output", int32(7)
			sub := Submission{LanguageID: "go1.27", Status: "done", Verdict: &verdict, Score: &score, CompileLog: &compileLog}
			// act
			report, err := svc.buildReport(t.Context(), sub, []TestRun{{
				Ordinal: 2, IsSample: tc.sample,
				Verdict: tc.verdict, TimeMS: 12, MemoryKB: 2048, CPUTimeMS: 7,
				InputKey: "in", ExpectedKey: "exp", StdoutKey: tc.stdout,
			}})
			// assert
			if err != nil {
				t.Fatal(err)
			}
			if report.GetLanguageId() != "go1.27" || report.GetScore() != 7 || report.GetCompileLog() != "" ||
				report.GetVerdict() != v1.Verdict_VERDICT_WRONG_ANSWER || len(report.GetTestRuns()) != 1 {
				t.Fatalf("report = %v", report)
			}
			run := report.GetTestRuns()[0]
			if run.GetOrdinal() != 2 || run.GetTimeMs() != 12 || run.GetMemoryKb() != 2048 || run.GetCpuTimeMs() != 7 ||
				run.GetIsTimeLimitExceeded() != tc.tl || run.GetIsMemoryLimitExceeded() != tc.ml ||
				(run.GetInput() != nil) != tc.details || (run.GetExpectedOutput() != nil) != tc.details ||
				(run.GetActualOutput() != nil) != tc.actual {
				t.Fatalf("run = %v", run)
			}
			if tc.actual && (run.GetDiff() != "-5\n+6\n" || run.GetInput().GetText() != "2 3\n") {
				t.Fatalf("details = %v", run)
			}
			if !tc.actual && run.GetDiff() != "" {
				t.Fatal("diff without actual output")
			}
		})
	}
	t.Run("compilation_error", func(t *testing.T) {
		verdict, log := "CE", "compile failed"
		report, err := NewService(nil, nil, nil).buildReport(t.Context(),
			Submission{Verdict: &verdict, CompileLog: &log}, nil)
		if err != nil || report.GetCompileLog() != log {
			t.Fatalf("report=%v err=%v", report, err)
		}
	})
}

func TestReportText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, data string
		truncated  bool
	}{
		{"empty", "", false},
		{"exact_limit", strings.Repeat("x", reportTextBytes), false},
		{"over_limit", strings.Repeat("x", reportTextBytes+1), true},
		{"split_unicode", strings.Repeat("я", reportTextBytes), true},
		{"invalid_utf8", "a\xffb", false},
		{"replacement_expands", strings.Repeat("\xffa", reportTextBytes/2), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := NewService(nil, &memObjects{data: map[string][]byte{"key": []byte(tc.data)}}, nil)
			text, err := svc.reportText(t.Context(), "key")
			if err != nil {
				t.Fatal(err)
			}
			if len(text.GetText()) > reportTextBytes || !utf8.ValidString(text.GetText()) || text.GetIsTruncated() != tc.truncated {
				t.Fatalf("text length=%d valid=%t truncated=%t", len(text.GetText()), utf8.ValidString(text.GetText()), text.GetIsTruncated())
			}
		})
	}
}

type brokenReportObjects struct{ Objects }

func (brokenReportObjects) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("storage unavailable")
}

func TestReportObjectError(t *testing.T) {
	svc := NewService(nil, brokenReportObjects{}, nil)
	_, err := svc.buildReport(t.Context(), Submission{}, []TestRun{{IsSample: true, Verdict: "WA"}})
	if err == nil {
		t.Fatal("storage error lost")
	}
}
