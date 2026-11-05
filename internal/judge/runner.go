package judge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/autocheck-team/backend/internal/sandbox"
)

// ErrNoTests не лечится ретраем: автор должен загрузить тесты.
var ErrNoTests = errors.New("problem has no ready tests")

// Workspace объявлен у потребителя, чтобы тестировать раннер без Docker.
type Workspace interface {
	AddFile(name string, data []byte, mode int64)
	Run(ctx context.Context, spec sandbox.RunSpec) (sandbox.Result, error)
	Close(ctx context.Context) error
}

// Sandbox изолирует создание контейнеров от логики проверки.
type Sandbox interface {
	EnsureImage(ctx context.Context, image string) error
	NewWorkspace(ctx context.Context) (Workspace, error)
}

// Docker адаптирует конкретную песочницу к интерфейсу потребителя.
type Docker struct{ Sandbox *sandbox.Sandbox }

// EnsureImage загружает образ до запуска контейнеров.
func (d Docker) EnsureImage(ctx context.Context, image string) error {
	return d.Sandbox.EnsureImage(ctx, image)
}

// NewWorkspace создаёт изолированный volume.
func (d Docker) NewWorkspace(ctx context.Context) (Workspace, error) {
	return d.Sandbox.NewWorkspace(ctx)
}

// Objects — только необходимые раннеру операции S3.
type Objects interface {
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
}

// Runs позволяет проверять переходы и результаты на стабах без БД.
type Runs interface {
	Start(ctx context.Context, j Job) error
	Load(ctx context.Context, j Job) (Task, error)
	Running(ctx context.Context, j Job) error
	SaveTest(ctx context.Context, j Job, t TestResult) error
}

// Runner выполняет одну попытку; повторами и арендой управляет Worker.
type Runner struct {
	repo    Runs
	objects Objects
	sandbox Sandbox
}

// NewRunner собирает зависимости проверки.
func NewRunner(repo Runs, objects Objects, sb Sandbox) *Runner {
	return &Runner{repo: repo, objects: objects, sandbox: sb}
}

func (r *Runner) image(ctx context.Context, image string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if err := r.sandbox.EnsureImage(ctx, image); err != nil {
		return fmt.Errorf("ensure image: %w", err)
	}
	return nil
}

func (r *Runner) read(ctx context.Context, key string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rc, err := r.objects.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", key, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("object %s exceeds %d bytes", key, limit)
	}
	return data, nil
}

func (r *Runner) workspace(ctx context.Context) (Workspace, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return r.sandbox.NewWorkspace(ctx)
}

func closeWorkspace(ctx context.Context, w Workspace) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = w.Close(ctx)
}

func runWorkspace(ctx context.Context, w Workspace, spec sandbox.RunSpec) (sandbox.Result, error) {
	// Внешний deadline покрывает также создание/копирование/удаление контейнера.
	ctx, cancel := context.WithTimeout(ctx, spec.TimeLimit+20*time.Second)
	defer cancel()
	return w.Run(ctx, spec)
}

// Run не кладёт эталон в workspace решения, даже для открытых тестов.
func (r *Runner) Run(ctx context.Context, j Job) (Outcome, error) {
	if err := r.repo.Start(ctx, j); err != nil {
		return Outcome{}, err
	}
	task, err := r.repo.Load(ctx, j)
	if err != nil {
		return Outcome{}, err
	}
	if len(task.Tests) == 0 {
		return Outcome{}, ErrNoTests
	}
	if err := r.image(ctx, task.Language.Image); err != nil {
		return Outcome{}, err
	}
	source, err := r.read(ctx, task.SourceKey, 64<<10)
	if err != nil {
		return Outcome{}, err
	}
	ws, err := r.workspace(ctx)
	if err != nil {
		return Outcome{}, fmt.Errorf("create solution workspace: %w", err)
	}
	defer closeWorkspace(ctx, ws)
	ws.AddFile(task.Language.SourceFile, source, 0o644)
	outcome := Outcome{Verdict: "OK"}
	if len(task.Language.CompileCmd) > 0 {
		result, err := runWorkspace(ctx, ws, sandbox.RunSpec{
			Image: task.Language.Image, Cmd: task.Language.CompileCmd,
			Env: task.Language.CompileEnv, TimeLimit: 30 * time.Second, MemoryBytes: 1 << 30, PidsLimit: 256,
		})
		if err != nil {
			return Outcome{}, fmt.Errorf("compile: %w", err)
		}
		log := append(bytes.Clone(result.Stdout), result.Stderr...)
		switch {
		case result.OOMKilled:
			log = append(log, "\nCompiler memory limit exceeded (1 GiB)."...)
		case result.TimedOut:
			log = append(log, "\nCompiler time limit exceeded (30s)."...)
		case result.OutputLimitExceeded:
			log = append(log, "\nCompiler output limit exceeded."...)
		}
		outcome.CompileLog = cleanTail(log, 64<<10)
		if result.ExitCode != 0 || result.OOMKilled || result.TimedOut || result.OutputLimitExceeded {
			outcome.Verdict = "CE"
			return outcome, nil
		}
	}
	var checker []byte
	if task.Problem.Checker == "custom" {
		if task.Problem.CheckerKey == nil {
			return Outcome{}, errors.New("custom checker key missing")
		}
		if err := r.image(ctx, "python:3.14-alpine"); err != nil {
			return Outcome{}, err
		}
		checker, err = r.read(ctx, *task.Problem.CheckerKey, 1<<20)
		if err != nil {
			return Outcome{}, err
		}
	}
	if err := r.repo.Running(ctx, j); err != nil {
		return Outcome{}, err
	}
	timeLimit := time.Duration(task.Problem.TimeLimitMS) * time.Millisecond
	memory := int64(task.Problem.MemoryLimitKB) * 1024
	for _, tc := range task.Tests {
		input, err := r.read(ctx, tc.InputKey, 64<<20)
		if err != nil {
			return Outcome{}, err
		}
		expected, err := r.read(ctx, tc.ExpectedKey, 64<<20)
		if err != nil {
			return Outcome{}, err
		}
		res, err := runWorkspace(ctx, ws, sandbox.RunSpec{
			Image: task.Language.Image, Cmd: task.Language.RunCmd,
			Stdin: input, TimeLimit: timeLimit, MemoryBytes: memory, PidsLimit: 256,
		})
		if err != nil {
			return Outcome{}, fmt.Errorf("run test %d: %w", tc.Ordinal, err)
		}
		verdict := Verdict(res, timeLimit, memory)
		if verdict == "OK" {
			if task.Problem.Checker == "custom" {
				verdict, err = r.custom(ctx, checker, input, expected, res.Stdout)
				if err != nil {
					return Outcome{}, err
				}
			} else {
				epsilon := 0.0
				if task.Problem.FloatEpsilon != nil {
					epsilon = *task.Problem.FloatEpsilon
				}
				if !Check(task.Problem.Checker, expected, res.Stdout, epsilon) {
					verdict = "WA"
				}
			}
		}
		tr := TestResult{
			TestID: tc.ID, Verdict: verdict, TimeMS: metric(res.WallTime.Milliseconds()),
			CPUTimeMS: metric(res.CPUTime.Milliseconds()), MemoryKB: metric((res.PeakMemoryBytes + 1023) / 1024),
			ExitCode: res.ExitCode, StderrTail: cleanTail(res.Stderr, 4096),
		}
		if tc.IsSample {
			key := fmt.Sprintf("submissions/%s/tests/%d.stdout", j.SubmissionID, tc.Ordinal)
			putCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := r.objects.Put(putCtx, key, bytes.NewReader(res.Stdout), int64(len(res.Stdout)), "application/octet-stream")
			cancel()
			if err != nil {
				return Outcome{}, fmt.Errorf("save sample stdout: %w", err)
			}
			tr.StdoutKey = &key
		}
		if err := r.repo.SaveTest(ctx, j, tr); err != nil {
			return Outcome{}, err
		}
		outcome.MaxTimeMS = max(outcome.MaxTimeMS, tr.TimeMS)
		outcome.MaxMemoryKB = max(outcome.MaxMemoryKB, tr.MemoryKB)
		if verdict == "OK" {
			outcome.Score = metric(int64(outcome.Score) + int64(tc.Points))
		} else if outcome.Verdict == "OK" {
			outcome.Verdict = verdict
		}
		if verdict != "OK" && task.Problem.StopOnFirstFailure {
			break
		}
	}
	return outcome, nil
}

func metric(n int64) int32 { return int32(min(max(n, 0), 1<<31-1)) }

func (r *Runner) custom(ctx context.Context, checker, input, expected, actual []byte) (string, error) {
	// Новый volume на каждый тест защищает и от оставленных решением файлов, и от
	// состояния предыдущего запуска чекера. Эталон никогда не монтируется решению.
	ws, err := r.workspace(ctx)
	if err != nil {
		return "", fmt.Errorf("create checker workspace: %w", err)
	}
	defer closeWorkspace(ctx, ws)
	ws.AddFile("checker.py", checker, 0o644)
	ws.AddFile("input", input, 0o644)
	ws.AddFile("expected", expected, 0o644)
	ws.AddFile("output", actual, 0o644)
	res, err := runWorkspace(ctx, ws, sandbox.RunSpec{
		Image: "python:3.14-alpine", Cmd: []string{"python3", "checker.py", "input", "expected", "output"},
		TimeLimit: 10 * time.Second, MemoryBytes: 256 << 20, PidsLimit: 256,
	})
	if err != nil {
		return "", fmt.Errorf("run custom checker: %w", err)
	}
	if res.OOMKilled || res.TimedOut || res.OutputLimitExceeded || (res.ExitCode != 0 && res.ExitCode != 1) {
		return "", fmt.Errorf("custom checker failed (exit=%d timeout=%t oom=%t): %s", res.ExitCode, res.TimedOut, res.OOMKilled, cleanTail(res.Stderr, 4096))
	}
	if res.ExitCode == 1 {
		return "WA", nil
	}
	return "OK", nil
}
