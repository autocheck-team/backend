package playground

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/autocheck-team/backend/internal/auth"
	"github.com/autocheck-team/backend/internal/sandbox"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

// TestDockerGatewayRuns проверяет реальную цепочку REST → gRPC → Postgres → worker → Docker.
func TestDockerGatewayRuns(t *testing.T) {
	f := setup(t)
	gateway := startGateway(t, f)
	sb, err := sandbox.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sb.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO subscriptions (user_id,plan_code,status,starts_at,ends_at)
 VALUES ($1,'pro','active',now()-interval '1 second',now()+interval '30 days')`, f.student.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	worker := Worker{
		Repository: f.repo, Runner: NewRunner(sb), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ID: "docker-test", Concurrency: 1, PollInterval: 10 * time.Millisecond,
	}
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(40 * time.Second):
			t.Error("worker did not stop")
		}
	})
	for _, test := range []struct {
		name, language, filename, source, stdin, verdict, stdout string
		extra                                                    []File
		truncated                                                bool
	}{
		{name: "python_hello", language: "python3.14", filename: "main.py", source: "print('hello')", verdict: "OK", stdout: "hello\n"},
		{name: "python_stdin", language: "python3.14", filename: "main.py", source: "print(input())", stdin: "from stdin\n", verdict: "OK", stdout: "from stdin\n"},
		{name: "python_timeout", language: "python3.14", filename: "main.py", source: "while True: pass", verdict: "TL"},
		{name: "python_runtime_error", language: "python3.14", filename: "main.py", source: "raise RuntimeError('bad')", verdict: "RE"},
		{name: "python_output_truncated", language: "python3.14", filename: "main.py", source: "print('x'*70000)", verdict: "OK", stdout: strings.Repeat("x", outputBytes), truncated: true},
		{name: "python_binary_output", language: "python3.14", filename: "main.py", source: "import sys;sys.stdout.buffer.write(b'hi\\x00\\xff')", verdict: "OK", stdout: "hi�"},
		{name: "cpp_hello_files", language: "cpp20-gcc15", filename: "main.cpp", source: "#include <iostream>\n#include \"hello.h\"\nint main(){std::cout<<greeting;}", extra: []File{{Name: "hello.h", Content: "const char* greeting=\"hello\";"}}, verdict: "OK", stdout: "hello"},
		{name: "cpp_stdin", language: "cpp20-gcc15", filename: "main.cpp", source: "#include <iostream>\nint main(){int n;std::cin>>n;std::cout<<n+1;}", stdin: "41\n", verdict: "OK", stdout: "42"},
		{name: "cpp_compilation_error", language: "cpp20-gcc15", filename: "main.cpp", source: "invalid C++", verdict: "CE"},
		{name: "cpp_timeout", language: "cpp20-gcc15", filename: "main.cpp", source: "int main(){for(;;){asm volatile(\"\");}}", verdict: "TL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// arrange
			files := []*v1.ProjectFile{{Name: test.filename, Content: test.source}}
			for _, file := range test.extra {
				files = append(files, &v1.ProjectFile{Name: file.Name, Content: file.Content})
			}
			var created v1.CreateProjectResponse
			gateway.call(t, f.student, "POST", "/v1/projects", &v1.CreateProjectRequest{Title: test.name, LanguageId: test.language, Files: files, ClientRequestId: uuid.NewString()}, &created, 200)
			var accepted v1.RunProjectResponse
			request := &v1.RunProjectRequest{Id: created.Project.Id, Stdin: test.stdin, ClientRequestId: uuid.NewString()}
			// act
			gateway.call(t, f.student, "POST", "/v1/projects/"+created.Project.Id+":run", request, &accepted, 200)
			result := waitRun(t, gateway, f.student, accepted.Run.Id)
			// assert
			if result.Verdict != test.verdict || result.Stdout != test.stdout || result.IsOutputTruncated != test.truncated {
				t.Fatalf("verdict=%s stdout_bytes=%d truncated=%t stderr=%q compile=%q", result.Verdict, len(result.Stdout), result.IsOutputTruncated, result.Stderr, result.CompileLog)
			}
			if result.Status != v1.ProjectRunStatus_PROJECT_RUN_STATUS_DONE || result.ExitCode == nil {
				t.Fatalf("unfinished result=%v", result)
			}
			if test.verdict == "CE" && result.CompileLog == "" {
				t.Fatal("missing compiler diagnostics")
			}
			if test.verdict == "TL" && result.TimeMs < 5000 {
				t.Fatalf("timeout too early: %dms", result.TimeMs)
			}
			gateway.call(t, f.other, "GET", "/v1/project-runs/"+result.Id, nil, nil, 404)
			gateway.call(t, f.admin, "GET", "/v1/project-runs/"+result.Id, nil, &v1.GetProjectRunResponse{}, 200)
			var repeated v1.RunProjectResponse
			gateway.call(t, f.student, "POST", "/v1/projects/"+created.Project.Id+":run", request, &repeated, 200)
			if repeated.Run.Id != result.Id {
				t.Fatal("retry created a new run")
			}
		})
	}
}

func waitRun(t *testing.T, gateway *gateway, principal auth.Principal, id string) *v1.ProjectRun {
	t.Helper()
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()
	for {
		var response v1.GetProjectRunResponse
		gateway.call(t, principal, "GET", "/v1/project-runs/"+id, nil, &response, 200)
		if response.Run.Status == v1.ProjectRunStatus_PROJECT_RUN_STATUS_DONE || response.Run.Status == v1.ProjectRunStatus_PROJECT_RUN_STATUS_FAILED {
			return response.Run
		}
		select {
		case <-deadline.C:
			t.Fatal("run did not finish")
			return nil
		case <-ticker.C:
		}
	}
}

func TestDockerWorkerShutdown(t *testing.T) {
	f := setup(t)
	sb, err := sandbox.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sb.Close(); err != nil {
			t.Error(err)
		}
	})
	project := f.create(t, f.student)
	if _, err := f.repo.Update(t.Context(), Project{
		ID: uuid.MustParse(project.Id), UserID: f.student.ID,
		Title: "shutdown", LanguageID: "python3.14", Files: []File{{Name: "main.py", Content: "while True: pass"}},
	}); err != nil {
		t.Fatal(err)
	}
	accepted := f.enqueue(t, project)
	worker := Worker{
		Repository: f.repo, Runner: NewRunner(sb), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ID: "shutdown-test", Concurrency: 1, PollInterval: 10 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(40 * time.Second):
			t.Error("worker remained running")
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	for {
		run, err := f.repo.GetRun(t.Context(), accepted.ID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == StatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not start run")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(40 * time.Second):
		t.Fatal("worker did not stop")
	}
	saved, err := f.repo.GetRun(t.Context(), accepted.ID)
	if err != nil || saved.Status != StatusQueued {
		t.Fatalf("cancelled run not queued: status=%v err=%v", saved.Status, err)
	}
	job, err := f.repo.Claim(t.Context(), "replacement")
	if err != nil || job.Attempts != 1 {
		t.Fatalf("shutdown consumed attempt: attempts=%d err=%v", job.Attempts, err)
	}
}
