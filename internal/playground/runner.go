package playground

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/autocheck-team/backend/internal/sandbox"
)

// Runner исполняет только сохранённые снимки с фиксированными ограничениями sandbox.
type Runner struct{ sandbox *sandbox.Sandbox }

// NewRunner создаёт исполнитель, доступный только процессу worker.
func NewRunner(sb *sandbox.Sandbox) *Runner { return &Runner{sandbox: sb} }

// Execute компилирует при необходимости и выполняет программу без тестовых эталонов.
func (r *Runner) Execute(ctx context.Context, run Run, stage func(context.Context, Status) error) (result Result, runErr error) {
	imageCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	err := r.sandbox.EnsureImage(imageCtx, run.Language.Image)
	cancel()
	if err != nil {
		return Result{}, fmt.Errorf("ensure playground image: %w", err)
	}
	workspaceCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	workspace, err := r.sandbox.NewWorkspace(workspaceCtx)
	cancel()
	if err != nil {
		return Result{}, fmt.Errorf("create playground workspace: %w", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		runErr = errors.Join(runErr, workspace.Close(cleanup))
	}()
	for _, file := range run.Files {
		workspace.AddFile(file.Name, []byte(file.Content), 0o644)
	}
	if len(run.Language.CompileCmd) > 0 {
		if err := stage(ctx, StatusCompiling); err != nil {
			return Result{}, err
		}
		compiled, err := execute(ctx, workspace, sandbox.RunSpec{
			Image: run.Language.Image, Cmd: run.Language.CompileCmd,
			Env: run.Language.CompileEnv, TimeLimit: 30 * time.Second, MemoryBytes: 1 << 30, PidsLimit: 256, OutputLimit: 1 << 20,
		})
		if err != nil {
			return Result{}, fmt.Errorf("compile project: %w", err)
		}
		result.CompileLog, result.Truncated = cleanOutput(append(append([]byte{}, compiled.Stdout...), compiled.Stderr...))
		result.Truncated = result.Truncated || compiled.OutputLimitExceeded
		if compiled.ExitCode != 0 || compiled.TimedOut || compiled.OOMKilled || compiled.OutputLimitExceeded {
			outcome := executionResult(compiled)
			outcome.Verdict = "CE"
			outcome.CompileLog = result.CompileLog
			outcome.Truncated = outcome.Truncated || result.Truncated
			return outcome, nil
		}
	}
	if err := stage(ctx, StatusRunning); err != nil {
		return Result{}, err
	}
	executed, err := execute(ctx, workspace, sandbox.RunSpec{
		Image: run.Language.Image, Cmd: run.Language.RunCmd,
		Stdin: []byte(run.Stdin), TimeLimit: 5 * time.Second, MemoryBytes: 256 << 20, PidsLimit: 256, OutputLimit: 1 << 20,
	})
	if err != nil {
		return Result{}, fmt.Errorf("execute project: %w", err)
	}
	outcome := executionResult(executed)
	outcome.CompileLog = result.CompileLog
	outcome.Truncated = outcome.Truncated || result.Truncated
	return outcome, nil
}

func execute(ctx context.Context, workspace *sandbox.Workspace, spec sandbox.RunSpec) (sandbox.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, spec.TimeLimit+30*time.Second)
	defer cancel()
	return workspace.Run(ctx, spec)
}

func cleanOutput(data []byte) (string, bool) {
	truncated := len(data) > outputBytes
	if truncated {
		data = data[:outputBytes]
	}
	text := strings.ToValidUTF8(string(data), "�")
	if len(text) > outputBytes {
		text = text[:outputBytes]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		truncated = true
	}
	// PostgreSQL text не допускает NUL, но произвольная программа может его вывести.
	return strings.ReplaceAll(text, "\x00", ""), truncated
}

func executionResult(result sandbox.Result) Result {
	exitCode := result.ExitCode
	if exitCode < math.MinInt32 || exitCode > math.MaxInt32 {
		exitCode = -1
	}
	code := int32(exitCode)
	stdout, stdoutTruncated := cleanOutput(result.Stdout)
	stderr, stderrTruncated := cleanOutput(result.Stderr)
	outcome := Result{
		Verdict: "OK", ExitCode: &code, Stdout: stdout, Stderr: stderr,
		Truncated: stdoutTruncated || stderrTruncated || result.OutputLimitExceeded,
		TimeMS:    result.WallTime.Milliseconds(), MemoryKB: result.PeakMemoryBytes / 1024,
	}
	switch {
	case result.TimedOut:
		outcome.Verdict = "TL"
	case result.OOMKilled:
		outcome.Verdict = "ML"
	case result.ExitCode != 0 || result.OutputLimitExceeded:
		outcome.Verdict = "RE"
	}
	return outcome
}
