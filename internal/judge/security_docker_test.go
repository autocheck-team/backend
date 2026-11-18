package judge

import (
	"context"
	"embed"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/autocheck-team/backend/internal/sandbox"
)

//go:embed testdata/security/*
var securitySources embed.FS

// securitySandbox наблюдает реальные запуски, сохраняя команды и лимиты Runner.
type securitySandbox struct {
	Docker
	t       *testing.T
	results []sandbox.Result
	closed  int
}

func (s *securitySandbox) NewWorkspace(ctx context.Context) (Workspace, error) {
	w, err := s.Docker.NewWorkspace(ctx)
	if err != nil {
		return nil, err
	}
	return &securityWorkspace{Workspace: w, owner: s}, nil
}

type securityWorkspace struct {
	Workspace
	owner *securitySandbox
}

func (w *securityWorkspace) Run(ctx context.Context, spec sandbox.RunSpec) (sandbox.Result, error) {
	result, err := w.Workspace.Run(ctx, spec)
	if err == nil {
		w.owner.results = append(w.owner.results, result)
	}
	return result, err
}

func (w *securityWorkspace) Close(ctx context.Context) error {
	err := w.Workspace.Close(ctx)
	if err != nil {
		w.owner.t.Errorf("workspace cleanup: %v", err)
	}
	w.owner.closed++
	return err
}

func securityDocker(t *testing.T) (*sandbox.Sandbox, *client.Client) {
	t.Helper()
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := cli.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	sb, err := sandbox.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sb.Close(); err != nil {
			t.Error(err)
		}
	})
	return sb, cli
}

// securityCanary остаётся в другом cgroup и PID namespace во время всех атак.
func securityCanary(t *testing.T, cli *client.Client) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	pids := int64(8)
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: "python:3.14-alpine", User: "65534:65534",
			Cmd: []string{"python3", "-c", "import time; time.sleep(600)"},
		},
		HostConfig: &container.HostConfig{
			NetworkMode: "none", ReadonlyRootfs: true, CapDrop: []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"}, LogConfig: container.LogConfig{Type: "none"},
			Resources: container.Resources{Memory: 32 << 20, MemorySwap: 32 << 20, PidsLimit: &pids},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := cli.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{Force: true}); err != nil {
			t.Error(err)
		}
	})
	if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	return created.ID
}

func TestDockerSandboxSecurity(t *testing.T) {
	sb, cli := securityDocker(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	err := sb.EnsureImage(ctx, "python:3.14-alpine")
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	canary := securityCanary(t, cli)
	pythonSource, err := securitySources.ReadFile("testdata/security/probe.py")
	if err != nil {
		t.Fatal(err)
	}
	nativeSource, err := securitySources.ReadFile("testdata/security/native.cpp")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, verdict, marker string
	}{
		{"fork_python", "RE", "PIDS_LIMIT"},
		{"fork_cpp", "RE", "PIDS_LIMIT"},
		{"fork_shell", "RE|TL", "fork"},
		{"tcp_external", "OK", ""},
		{"tcp_gateway", "OK", ""},
		{"tcp_desktop_host", "OK", ""},
		{"tcp_metadata", "OK", ""},
		{"tcp_ipv6", "OK", ""},
		{"dns", "OK", ""},
		{"files", "OK", ""},
		{"write_rootfs", "OK", ""},
		{"workspace_full", "RE", "WORKSPACE_FULL"},
		{"inodes", "RE", "INODE_LIMIT"},
		{"tmp_full", "RE", "TMP_FULL"},
		{"file_size", "RE", "FILE_SIZE_LIMIT"},
		{"stdout", "RE", ""},
		{"stderr", "RE", ""},
		{"mixed_output", "RE", ""},
		{"memory", "ML", ""},
		{"cpu", "TL", ""},
		{"wall_sleep", "TL", ""},
		{"threads", "RE", "THREAD_LIMIT"},
		{"privileges_cpp", "OK", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// arrange
			repo, objects, _, _ := runnerFixture()
			repo.task.Tests = repo.task.Tests[:1]
			repo.task.Problem.TimeLimitMS = 3000
			repo.task.Problem.MemoryLimitKB = 128 * 1024
			repo.task.Language = Language{Image: "python:3.14-alpine", SourceFile: "main.py", RunCmd: []string{"python3", "main.py"}}
			source := fmt.Sprintf("scenario = %q\n", tc.name) + string(pythonSource)
			if strings.HasSuffix(tc.name, "_cpp") {
				repo.task.Language = Language{
					Image: "gcc:15", SourceFile: "main.cpp", CompileEnv: []string{"TMPDIR=/work/box"},
					CompileCmd: []string{"g++", "-std=c++20", "-O2", "-o", "solution", "main.cpp"},
					RunCmd:     []string{"./solution"},
				}
				source = string(nativeSource)
				if tc.name == "fork_cpp" {
					source = "#define FORK_BOMB\n" + source
				}
			}
			if tc.name == "workspace_full" || tc.name == "file_size" {
				// Квоту tmpfs проверяем отдельно от cgroup OOM и RLIMIT_FSIZE.
				repo.task.Problem.MemoryLimitKB = 768 * 1024
				repo.task.Problem.TimeLimitMS = 10000
			}
			if tc.name == "file_size" {
				source = "import errno,sys\ntry:\n with open('big', 'wb', buffering=0) as f:\n  while True: f.write(b'x'*65536)\nexcept OSError as e:\n assert e.errno == errno.EFBIG\n print('FILE_SIZE_LIMIT', file=sys.stderr)\n sys.exit(2)\n"
			}
			if tc.name == "inodes" {
				repo.task.Problem.TimeLimitMS = 10000
			}
			if tc.name == "fork_python" {
				// Достаточная память позволяет отдельно доказать срабатывание pids.max.
				repo.task.Problem.MemoryLimitKB = 768 * 1024
			}
			if tc.name == "dns" {
				repo.task.Problem.TimeLimitMS = 10000
			}
			if tc.name == "files" {
				// Эталон и чекер существуют в хранилище и действительно используются после решения.
				key := "checker"
				repo.task.Problem.Checker, repo.task.Problem.CheckerKey = "custom", &key
				objects.data[key] = []byte("import sys\nfrom pathlib import Path\nassert Path(sys.argv[2]).read_text() == 'private-reference'\nassert Path(sys.argv[3]).read_text().strip() == '5'\n")
				objects.data["exp"] = []byte("private-reference")
			}
			objects.data["source"] = []byte(source)
			observed := &securitySandbox{Docker: Docker{Sandbox: sb}, t: t}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)

			// act
			out, err := NewRunner(repo, objects, observed).Run(t.Context(), Job{SubmissionID: uuid.New()})
			runtime.ReadMemStats(&after)

			// assert
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains("|"+tc.verdict+"|", "|"+out.Verdict+"|") || len(repo.runs) != 1 {
				t.Fatalf("verdict=%s want=%s compile=%q runs=%+v", out.Verdict, tc.verdict, out.CompileLog, repo.runs)
			}
			wantClosed := 1
			if tc.name == "files" {
				wantClosed++
			}
			if observed.closed != wantClosed {
				t.Fatalf("closed workspaces=%d want=%d", observed.closed, wantClosed)
			}
			result := observed.results[len(observed.results)-1]
			if tc.marker != "" && !strings.Contains(string(result.Stderr), tc.marker) {
				t.Fatalf("limit not observed: marker=%s stderr=%q", tc.marker, result.Stderr)
			}
			outputBytes := len(result.Stdout) + len(result.Stderr)
			if tc.name == "stdout" || tc.name == "stderr" || tc.name == "mixed_output" {
				if !result.OutputLimitExceeded || outputBytes != sandbox.DefaultOutputLimit {
					t.Fatalf("exceeded=%t output=%d", result.OutputLimitExceeded, outputBytes)
				}
				// TotalAlloc замечает и освобождённые буферы: непрерывный вывод не должен их плодить.
				allocated := after.TotalAlloc - before.TotalAlloc
				if allocated > 256<<20 {
					t.Fatalf("output allocated %d bytes", allocated)
				}
				t.Logf("worker heap before=%d after=%d allocated=%d output=%d", before.HeapAlloc, after.HeapAlloc, allocated, outputBytes)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			info, err := cli.ContainerInspect(ctx, canary, client.ContainerInspectOptions{})
			if err != nil || !info.Container.State.Running {
				t.Fatalf("neighbor container affected: %v", err)
			}
			t.Logf("verdict=%s exit=%d wall=%s memory=%d output=%d neighbor=running", out.Verdict, result.ExitCode, result.WallTime, result.PeakMemoryBytes, outputBytes)
		})
	}
}
