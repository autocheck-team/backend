package judge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/autocheck-team/backend/internal/problem"
	"github.com/autocheck-team/backend/internal/sandbox"
)

type fakeRuns struct {
	task             Task
	runs             []TestResult
	started, running bool
}

func (f *fakeRuns) Start(context.Context, Job) error        { f.started = true; f.runs = nil; return nil }
func (f *fakeRuns) Load(context.Context, Job) (Task, error) { return f.task, nil }
func (f *fakeRuns) Running(context.Context, Job) error      { f.running = true; return nil }
func (f *fakeRuns) SaveTest(_ context.Context, _ Job, tr TestResult) error {
	f.runs = append(f.runs, tr)
	return nil
}

type fakeObjects struct {
	data map[string][]byte
	fail bool
}

func (f *fakeObjects) Get(_ context.Context, key string) (io.ReadCloser, error) {
	if f.fail {
		return nil, errors.New("S3 unavailable")
	}
	data, ok := f.data[key]
	if !ok {
		return nil, errors.New("object missing")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *fakeObjects) Put(_ context.Context, key string, reader io.Reader, _ int64, _ string) error {
	b, err := io.ReadAll(reader)
	if err == nil {
		f.data[key] = b
	}
	return err
}

type fakeWorkspace struct {
	files   map[string][]byte
	specs   []sandbox.RunSpec
	results []sandbox.Result
	err     error
	closed  bool
}

func (f *fakeWorkspace) AddFile(name string, data []byte, _ int64) { f.files[name] = bytes.Clone(data) }

func (f *fakeWorkspace) Run(_ context.Context, spec sandbox.RunSpec) (sandbox.Result, error) {
	f.specs = append(f.specs, spec)
	if f.err != nil {
		return sandbox.Result{}, f.err
	}
	if len(f.results) == 0 {
		return sandbox.Result{}, errors.New("unexpected run")
	}
	res := f.results[0]
	f.results = f.results[1:]
	return res, nil
}
func (f *fakeWorkspace) Close(context.Context) error { f.closed = true; return nil }

type fakeSandbox struct {
	workspaces []*fakeWorkspace
	next       int
}

func (*fakeSandbox) EnsureImage(context.Context, string) error { return nil }
func (f *fakeSandbox) NewWorkspace(context.Context) (Workspace, error) {
	if f.next == len(f.workspaces) {
		return nil, errors.New("unexpected workspace")
	}
	ws := f.workspaces[f.next]
	f.next++
	return ws, nil
}

func runnerFixture(results ...sandbox.Result) (*fakeRuns, *fakeObjects, *fakeWorkspace, *fakeSandbox) {
	repo := &fakeRuns{task: Task{
		SourceKey: "source", Language: Language{Image: "toolchain", SourceFile: "main", RunCmd: []string{"./solution"}},
		Problem: problem.Problem{Checker: "tokens", TimeLimitMS: 1000, MemoryLimitKB: 65536},
		Tests:   []problem.TestCase{{ID: uuid.New(), Ordinal: 1, InputKey: "in", ExpectedKey: "exp", Points: 2, IsSample: true}, {ID: uuid.New(), Ordinal: 2, InputKey: "in", ExpectedKey: "exp", Points: 3}},
	}}
	objects := &fakeObjects{data: map[string][]byte{"source": []byte("solution"), "in": []byte("2 3"), "exp": []byte("5")}}
	ws := &fakeWorkspace{files: map[string][]byte{}, results: results}
	return repo, objects, ws, &fakeSandbox{workspaces: []*fakeWorkspace{ws}}
}

func TestRunnerVerdictScoreAndPrivacy(t *testing.T) {
	repo, objects, ws, sb := runnerFixture(sandbox.Result{Stdout: []byte("4"), Stderr: []byte("x\x00\xffy")}, sandbox.Result{Stdout: []byte("5")})
	out, err := NewRunner(repo, objects, sb).Run(t.Context(), Job{SubmissionID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	if out.Verdict != "WA" || out.Score != 3 || len(repo.runs) != 2 || !repo.started || !repo.running || !ws.closed {
		t.Fatalf("outcome=%+v runs=%+v", out, repo.runs)
	}
	if len(ws.files) != 1 || string(ws.specs[0].Stdin) != "2 3" {
		t.Fatal("expected/source privacy violated")
	}
	if repo.runs[0].StdoutKey == nil || repo.runs[1].StdoutKey != nil || repo.runs[0].StderrTail != "xy" {
		t.Fatalf("artifacts=%+v", repo.runs)
	}
}

func TestRunnerStopAndCompileError(t *testing.T) {
	for _, compilation := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop", true: "CE"}[compilation], func(t *testing.T) {
			repo, objects, ws, sb := runnerFixture(sandbox.Result{ExitCode: 1, Stderr: []byte("bad\x00\xff")})
			repo.task.Problem.StopOnFirstFailure = true
			if compilation {
				repo.task.Language.CompileCmd = []string{"compile"}
				repo.task.Language.CompileEnv = []string{"TMPDIR=/work/box"}
			}
			out, err := NewRunner(repo, objects, sb).Run(t.Context(), Job{})
			if err != nil {
				t.Fatal(err)
			}
			if compilation {
				if out.Verdict != "CE" || out.CompileLog != "bad" || len(repo.runs) != 0 || repo.running || ws.specs[0].PidsLimit != 256 {
					t.Fatalf("CE=%+v", out)
				}
			} else if out.Verdict != "RE" || len(repo.runs) != 1 {
				t.Fatalf("stop=%+v", out)
			}
		})
	}
}

func TestRunnerSystemErrors(t *testing.T) {
	for _, kind := range []string{"no_tests", "S3", "Docker", "compile_timeout", "compile_oom"} {
		t.Run(kind, func(t *testing.T) {
			repo, objects, ws, sb := runnerFixture(sandbox.Result{TimedOut: kind == "compile_timeout", OOMKilled: kind == "compile_oom"})
			switch kind {
			case "no_tests":
				repo.task.Tests = nil
			case "S3":
				objects.fail = true
			case "Docker":
				ws.err = errors.New("docker unavailable")
			default:
				repo.task.Language.CompileCmd = []string{"compile"}
			}
			out, err := NewRunner(repo, objects, sb).Run(t.Context(), Job{})
			if kind == "compile_timeout" || kind == "compile_oom" {
				if err != nil || out.Verdict != "CE" || out.CompileLog == "" {
					t.Fatalf("out=%+v err=%v", out, err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected system error")
			}
			if kind == "no_tests" && !errors.Is(err, ErrNoTests) {
				t.Fatal(err)
			}
		})
	}
}

func TestRunnerCustomSeparateWorkspace(t *testing.T) {
	for _, code := range []int{0, 1, 2} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			repo, objects, solution, sb := runnerFixture(sandbox.Result{Stdout: []byte("5")})
			repo.task.Tests = repo.task.Tests[:1]
			repo.task.Problem.Checker = "custom"
			key := "checker"
			repo.task.Problem.CheckerKey = &key
			objects.data[key] = []byte("script")
			checker := &fakeWorkspace{files: map[string][]byte{}, results: []sandbox.Result{{ExitCode: code}}}
			sb.workspaces = append(sb.workspaces, checker)
			out, err := NewRunner(repo, objects, sb).Run(t.Context(), Job{})
			if code == 2 {
				if err == nil {
					t.Fatal("expected checker SE")
				}
			} else if err != nil || out.Verdict != map[int]string{0: "OK", 1: "WA"}[code] {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			if len(solution.files) != 1 || len(checker.files) != 4 || !checker.closed {
				t.Fatal("checker isolation violated")
			}
		})
	}
}
