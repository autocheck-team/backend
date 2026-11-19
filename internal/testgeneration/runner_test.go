package testgeneration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"

	"github.com/autocheck-team/backend/internal/llm"
	"github.com/autocheck-team/backend/internal/sandbox"
)

type modelStub struct {
	content json.RawMessage
	err     error
}

func (m modelStub) Chat(context.Context, []llm.Message, llm.Options) (json.RawMessage, error) {
	return m.content, m.err
}

func TestInvalidModelOutput(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"malformed", `{`}, {"empty", `{"tests":[]}`}, {"excess", `{"tests":[{},{}]}`}, {"unknown_fields", `{"tests":[{}],"unexpected":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// arrange
			runner := NewRunner(modelStub{content: json.RawMessage(tc.content)}, nil)
			// act
			_, err := runner.Run(t.Context(), Task{TestCount: 1})
			// assert
			if err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}
	runner := NewRunner(modelStub{err: errors.New("model unavailable")}, nil)
	if _, err := runner.Run(t.Context(), Task{TestCount: 1}); err == nil {
		t.Fatal("model error ignored")
	}
}

func TestDockerCandidateLimits(t *testing.T) {
	// arrange
	sb, err := sandbox.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := sb.Close(); err != nil {
			t.Error(err)
		}
	}()
	candidates := []candidate{
		{Input: "1", Generator: "print(1)", Explanation: "ambiguous"},
		{Input: strings.Repeat("a", maxData+1), Explanation: "oversized"},
		{Generator: strings.Repeat("x", 64<<10+1), Explanation: "oversized generator"},
		{Generator: "while True: pass", Explanation: "generator TL"},
		{Input: "1", Explanation: strings.Repeat("x", 16001)},
		{Input: "1", Explanation: "empty output is valid"},
	}
	content, err := json.Marshal(map[string]any{"tests": candidates})
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(modelStub{content: content}, sb)
	task := Task{Source: "pass", TestCount: 6, TimeLimitMS: 500, MemoryLimitKB: 65536, Language: Language{ID: "python3.14", Image: "python:3.14-alpine", SourceFile: "main.py", RunCmd: []string{"python3", "main.py"}}}
	// act
	drafts, err := runner.Run(t.Context(), task)
	// assert
	if err != nil {
		t.Fatal(err)
	}
	for index, draft := range drafts {
		if (draft.RejectionReason == "") != (index == 5) {
			t.Fatalf("unexpected candidate %d reason %q", index, draft.RejectionReason)
		}
	}
	if drafts[5].Expected == nil || len(drafts[5].Expected) != 0 {
		t.Fatal("empty stdout must remain non-null")
	}
}

func TestDockerReferenceFailures(t *testing.T) {
	// arrange
	sb, err := sandbox.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := sb.Close(); err != nil {
			t.Error(err)
		}
	}()
	content := json.RawMessage(`{"tests":[{"input":"1","explanation":"failure"}]}`)
	runner := NewRunner(modelStub{content: content}, sb)
	for _, tc := range []struct{ name, source, reason string }{
		{"timeout", "while True: pass", "time limit exceeded"},
		{"output_limit", "print('x'*(2**21))", "output exceeds 1 MiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := Task{Source: tc.source, TestCount: 1, TimeLimitMS: 200, MemoryLimitKB: 65536, Language: Language{Image: "python:3.14-alpine", SourceFile: "main.py", RunCmd: []string{"python3", "main.py"}}}
			drafts, err := runner.Run(t.Context(), task)
			if err != nil || len(drafts) != 1 || drafts[0].RejectionReason != tc.reason {
				t.Fatalf("unexpected reference outcome: err=%v", err)
			}
		})
	}
}

func TestWorkerFailureAndRelease(t *testing.T) {
	// arrange
	f := setup(t)
	f.pro(t, f.author)
	f.create(t, f.author, f.request())
	runner := NewRunner(modelStub{err: errors.New("model unavailable")}, nil)
	job, err := f.repo.Claim(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var reported []error
	// act
	f.repo.process(t.Context(), runner, job, func(err error) { reported = append(reported, err) })
	// assert
	result, err := f.repo.Get(t.Context(), f.problemID, job.ID)
	if err != nil || result.Status != 4 || result.Error == "" || len(reported) != 1 {
		t.Fatal("worker failure not saved")
	}
	f.create(t, f.author, f.request())
	job, err = f.repo.Claim(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.Heartbeat(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	f.repo.process(ctx, runner, job, func(err error) { t.Error(err) })
	result, err = f.repo.Get(t.Context(), f.problemID, job.ID)
	if err != nil || result.Status != 1 {
		t.Fatal("cancelled job not released")
	}
	reclaimed, err := f.repo.Claim(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.Attempts != 1 {
		t.Fatal("cancelled attempt consumed retry")
	}
	if err := f.repo.Finish(t.Context(), reclaimed, nil, "attempt limit"); err != nil {
		t.Fatal(err)
	}
}

func TestLargeDraftThroughGateway(t *testing.T) {
	// arrange
	f := setup(t)
	f.pro(t, f.author)
	f.create(t, f.author, f.request())
	job, err := f.repo.Claim(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	drafts := make([]Draft, 5)
	for i := range drafts {
		drafts[i] = Draft{Input: []byte(strings.Repeat("x", maxData)), Expected: []byte{}, Explanation: "large"}
	}
	if err := f.repo.Finish(t.Context(), job, drafts, ""); err != nil {
		t.Fatal(err)
	}
	gateway := startGateway(t, f)
	// act
	result := &v1.GetTestGenerationResponse{}
	gateway.call(t, f.author, http.MethodGet, "/v1/problems/"+f.problemID.String()+"/test-generations/"+job.ID.String(), nil, result, http.StatusOK)
	// assert
	if len(result.GetGeneration().GetTests()) != 5 || len(result.Generation.Tests[4].Input) != maxData {
		t.Fatal("large draft response truncated")
	}
}
