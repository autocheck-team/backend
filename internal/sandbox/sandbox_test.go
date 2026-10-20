package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

const (
	alpine = "alpine:3.22"
	python = "python:3.14-alpine"
	mb     = 1 << 20
)

// newWorkspace создаёт песочницу на локальном Docker или пропускает тест, если его нет.
func newWorkspace(t *testing.T, images ...string) *Workspace {
	t.Helper()
	sb, err := New("")
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	if _, err := sb.cli.Ping(ctx, client.PingOptions{}); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	for _, img := range images {
		if err := sb.EnsureImage(ctx, img); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := sb.NewWorkspace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ws.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return ws
}

func sh(script string) RunSpec {
	return RunSpec{Image: alpine, Cmd: []string{"sh", "-c", script}, TimeLimit: 10 * time.Second, MemoryBytes: 64 * mb}
}

func run(t *testing.T, ws *Workspace, spec RunSpec) Result {
	t.Helper()
	res, err := ws.Run(t.Context(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func TestRunProgramWithStdinAndFiles(t *testing.T) {
	ws := newWorkspace(t, python)
	ws.AddFile("main.py", []byte("a, b = map(int, input().split())\nprint(a + b)\n"), 0o644)

	res := run(t, ws, RunSpec{
		Image: python, Cmd: []string{"python3", "main.py"}, Stdin: []byte("2 3\n"),
		TimeLimit: 10 * time.Second, MemoryBytes: 128 * mb,
	})
	if res.ExitCode != 0 || strings.TrimSpace(string(res.Stdout)) != "5" {
		t.Fatalf("exit=%d stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}
}

func TestWorkspaceSharedBetweenRuns(t *testing.T) {
	ws := newWorkspace(t, alpine)
	// «Компиляция» пишет файл в рабочий каталог, следующий запуск его видит.
	if res := run(t, ws, sh("echo built > artifact")); res.ExitCode != 0 {
		t.Fatalf("first run: exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	res := run(t, ws, sh("cat artifact"))
	if strings.TrimSpace(string(res.Stdout)) != "built" {
		t.Fatalf("stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

func TestIsolation(t *testing.T) {
	ws := newWorkspace(t, alpine)

	tests := []struct {
		name   string
		script string
		check  func(t *testing.T, r Result)
	}{
		{"runs_as_nobody", "id -u; id -g", func(t *testing.T, r Result) {
			if got := strings.Fields(string(r.Stdout)); len(got) != 2 || got[0] != "65534" || got[1] != "65534" {
				t.Fatalf("uid/gid = %q", r.Stdout)
			}
		}},
		{"no_network", "wget -q -T 2 -O- http://1.1.1.1 && echo CONNECTED", func(t *testing.T, r Result) {
			if r.ExitCode == 0 || strings.Contains(string(r.Stdout), "CONNECTED") {
				t.Fatalf("network is reachable: exit=%d out=%q", r.ExitCode, r.Stdout)
			}
		}},
		// В network namespace ядро всегда создаёт выключенные туннельные интерфейсы
		// (gre0, sit0 и т.п.), поэтому проверяем не список интерфейсов, а отсутствие маршрутов.
		{"no_routes", "tail -n +2 /proc/net/route", func(t *testing.T, r Result) {
			if strings.TrimSpace(string(r.Stdout)) != "" {
				t.Fatalf("routes = %q", r.Stdout)
			}
		}},
		{"rootfs_read_only", "touch /etc/pwned", func(t *testing.T, r Result) {
			if r.ExitCode == 0 {
				t.Fatal("wrote to /etc")
			}
		}},
		{"no_capabilities", "grep CapEff /proc/self/status", func(t *testing.T, r Result) {
			if !strings.Contains(string(r.Stdout), "0000000000000000") {
				t.Fatalf("effective capabilities: %q", r.Stdout)
			}
		}},
		{"no_new_privileges", "grep NoNewPrivs /proc/self/status", func(t *testing.T, r Result) {
			if !strings.Contains(string(r.Stdout), "1") {
				t.Fatalf("NoNewPrivs: %q", r.Stdout)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.check(t, run(t, ws, sh(tt.script))) })
	}
}

func TestLimits(t *testing.T) {
	ws := newWorkspace(t, alpine)

	t.Run("time_limit_kills", func(t *testing.T) {
		spec := sh("sleep 30")
		spec.TimeLimit = time.Second
		res := run(t, ws, spec)
		if !res.TimedOut || res.WallTime > 5*time.Second {
			t.Fatalf("timedOut=%v wall=%s", res.TimedOut, res.WallTime)
		}
	})
	t.Run("memory_limit_oom", func(t *testing.T) {
		// tail /dev/zero копит бесконечную «строку» в памяти.
		spec := sh("tail /dev/zero")
		spec.MemoryBytes = 32 * mb
		res := run(t, ws, spec)
		if !res.OOMKilled && res.ExitCode != 137 {
			t.Fatalf("expected OOM kill: oom=%v exit=%d timedOut=%v", res.OOMKilled, res.ExitCode, res.TimedOut)
		}
	})
	t.Run("fork_bomb_contained", func(t *testing.T) {
		spec := sh(`i=0; while [ $i -lt 500 ]; do sleep 5 & i=$((i+1)); done; wait`)
		spec.TimeLimit = 5 * time.Second
		res := run(t, ws, spec)
		if !strings.Contains(string(res.Stderr), "can't fork") && !strings.Contains(string(res.Stderr), "Resource") {
			t.Fatalf("pids limit not hit: exit=%d stderr=%q", res.ExitCode, res.Stderr)
		}
	})
	t.Run("output_limit", func(t *testing.T) {
		spec := sh("yes")
		spec.OutputLimit = 1 * mb
		res := run(t, ws, spec)
		if !res.OutputLimitExceeded || len(res.Stdout) != 1*mb {
			t.Fatalf("exceeded=%v stdout=%d bytes", res.OutputLimitExceeded, len(res.Stdout))
		}
	})
	t.Run("file_size_limit", func(t *testing.T) {
		spec := sh("head -c 10485760 /dev/zero > big")
		spec.FileSizeLimit = 1 * mb
		res := run(t, ws, spec)
		if res.ExitCode == 0 {
			t.Fatal("wrote file over fsize limit")
		}
	})
	t.Run("exit_code_reported", func(t *testing.T) {
		if res := run(t, ws, sh("exit 3")); res.ExitCode != 3 {
			t.Fatalf("exit=%d", res.ExitCode)
		}
	})
}
