package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

func TestDockerWorkspaceQuotaRegression(t *testing.T) {
	bounded := newWorkspace(t, python)
	legacy, err := bounded.sb.cli.VolumeCreate(t.Context(), client.VolumeCreateOptions{
		Labels: map[string]string{labelKey: "workspace"},
	})
	if err != nil {
		t.Fatal(err)
	}
	unbounded := &Workspace{sb: bounded.sb, volume: legacy.Volume.Name, pending: map[string]file{}}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := unbounded.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	// Старую настройку проверяем конечными 320 МиБ, не заполняя диск хоста.
	source := "import errno,os\nif os.statvfs('.').f_bavail*os.statvfs('.').f_frsize < 512*1024*1024 and os.statvfs('.').f_blocks*os.statvfs('.').f_frsize > 256*1024*1024: raise SystemExit(77)\nwritten=0\ntry:\n for i in range(40):\n  with open(str(i),'wb',buffering=0) as f:\n   for j in range(8): written+=f.write(b'x'*1048576)\nexcept OSError as e:\n assert e.errno==errno.ENOSPC\nprint(written)\n"
	written := make([]int64, 0, 2)
	for _, ws := range []*Workspace{unbounded, bounded} {
		ws.AddFile("main.py", []byte(source), 0o644)
		result := run(t, ws, RunSpec{
			Image: python, Cmd: []string{"python3", "main.py"}, TimeLimit: 10 * time.Second, MemoryBytes: 768 * mb,
		})
		if result.ExitCode == 77 {
			t.Skip("not enough free space for bounded legacy comparison")
		}
		if result.ExitCode != 0 {
			t.Fatalf("quota comparison: exit=%d stderr=%q", result.ExitCode, result.Stderr)
		}
		count, err := strconv.ParseInt(strings.TrimSpace(string(result.Stdout)), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		written = append(written, count)
	}
	if written[0] != 320*mb || written[1] >= WorkspaceBytes || written[1] < WorkspaceBytes-mb {
		t.Fatalf("written bytes: legacy=%d bounded=%d", written[0], written[1])
	}
	t.Logf("legacy accepted=%d bytes; bounded accepted=%d bytes then ENOSPC", written[0], written[1])
}

func TestDockerRootOwnedSetuidDenied(t *testing.T) {
	ws := newWorkspace(t, "gcc:15")
	ws.AddFile("suid.c", []byte("#include <unistd.h>\n#include <stdio.h>\nint main(void){printf(\"%d\\n\",(int)geteuid());return geteuid()==65534?0:2;}\n"), 0o644)
	spec := RunSpec{Image: "gcc:15", Cmd: []string{"gcc", "-o", "suid", "suid.c"}, TimeLimit: 10 * time.Second, MemoryBytes: 256 * mb}
	if result := run(t, ws, spec); result.ExitCode != 0 {
		t.Fatalf("compile setuid fixture: %q", result.Stderr)
	}
	id, err := ws.create(t.Context(), withDefaults(spec), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := ws.sb.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true}); err != nil {
			t.Error(err)
		}
	})
	archive, err := ws.sb.cli.CopyFromContainer(t.Context(), id, client.CopyFromContainerOptions{SourcePath: WorkDir + "/suid"})
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(archive.Content)
	if _, err := reader.Next(); err != nil {
		t.Fatal(err)
	}
	binary, readErr := io.ReadAll(io.LimitReader(reader, 4<<20))
	if err := archive.Content.Close(); err != nil {
		t.Fatal(err)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	// Только доверенная подготовка теста назначает root-владельца: решение chown не умеет.
	if err := writer.WriteHeader(&tar.Header{Name: "suid", Mode: 0o4755, Uid: 0, Gid: 0, Size: int64(len(binary))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.sb.cli.CopyToContainer(t.Context(), id, client.CopyToContainerOptions{
		DestinationPath: WorkDir, Content: &buf,
	}); err != nil {
		t.Fatal(err)
	}
	spec.Cmd = []string{"sh", "-c", `stat -c '%u %a' suid; ./suid`}
	result := run(t, ws, spec)
	if result.ExitCode != 0 || strings.TrimSpace(string(result.Stdout)) != "0 4755\n65534" {
		t.Fatalf("setuid fixture: exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
}

func TestDockerSecurityConfiguration(t *testing.T) {
	ws := newWorkspace(t, python)
	spec := withDefaults(RunSpec{Image: python, Cmd: []string{"python3", "main.py"}, TimeLimit: time.Second, MemoryBytes: 128 * mb})
	id, err := ws.create(t.Context(), spec, "run")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := ws.sb.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true}); err != nil {
			t.Error(err)
		}
	})
	info, err := ws.sb.cli.ContainerInspect(t.Context(), id, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	host, cfg := info.Container.HostConfig, info.Container.Config
	if cfg.User != nobody || !cfg.NetworkDisabled || host.NetworkMode != "none" ||
		!host.ReadonlyRootfs || host.Privileged || host.PidMode != "" || host.IpcMode.IsHost() ||
		!slices.Contains(host.CapDrop, "ALL") || len(host.CapAdd) != 0 ||
		!slices.Contains(host.SecurityOpt, "no-new-privileges") ||
		slices.Contains(host.SecurityOpt, "seccomp=unconfined") ||
		!slices.Contains(host.MaskedPaths, "/proc/1/environ") ||
		host.Init == nil || !*host.Init || host.LogConfig.Type != "none" {
		t.Fatal("unsafe container isolation configuration")
	}
	if host.Memory != spec.MemoryBytes || host.MemorySwap != spec.MemoryBytes ||
		host.PidsLimit == nil || *host.PidsLimit != spec.PidsLimit || host.NanoCPUs != 1e9 {
		t.Fatal("missing resource limits")
	}
	for name, want := range map[string]int64{"fsize": DefaultFileSizeLimit, "nofile": 256, "core": 0, "cpu": 2} {
		index := slices.IndexFunc(host.Ulimits, func(limit *container.Ulimit) bool { return limit.Name == name })
		if index < 0 || host.Ulimits[index].Soft != want || host.Ulimits[index].Hard != want {
			t.Fatalf("missing %s limit", name)
		}
	}
	if len(info.Container.Mounts) != 1 || info.Container.Mounts[0].Name != ws.volume || info.Container.Mounts[0].Destination != "/work" {
		t.Fatal("unexpected mounts in solution")
	}
	volume, err := ws.sb.cli.VolumeInspect(t.Context(), ws.volume, client.VolumeInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if volume.Volume.Driver != "local" || volume.Volume.Options["type"] != "tmpfs" {
		t.Fatal("workspace is disk backed")
	}
	for _, option := range []string{fmt.Sprintf("size=%d", WorkspaceBytes), "nr_inodes=16384", "nosuid", "nodev", "mode=0755"} {
		if !slices.Contains(strings.Split(volume.Volume.Options["o"], ","), option) {
			t.Fatalf("workspace missing %s", option)
		}
	}
}

func TestDockerSecurityCleanup(t *testing.T) {
	base := newWorkspace(t, python)
	for _, cancellation := range []bool{false, true} {
		name := "fork_descendants"
		if cancellation {
			name = "cancel_running"
		}
		t.Run(name, func(t *testing.T) {
			ws, err := base.sb.NewWorkspace(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := ws.Close(ctx); err != nil {
					t.Fatal(err)
				}
				containers, err := ws.sb.cli.ContainerList(ctx, client.ContainerListOptions{
					All: true, Filters: client.Filters{}.Add("volume", ws.volume),
				})
				if err != nil || len(containers.Items) != 0 {
					t.Fatalf("containers left after cleanup: count=%d err=%v", len(containers.Items), err)
				}
				if _, err := ws.sb.cli.VolumeInspect(ctx, ws.volume, client.VolumeInspectOptions{}); err == nil {
					t.Fatal("workspace volume left after cleanup")
				}
			})
			source := "import errno,os,time\nfrom pathlib import Path\np=Path('/sys/fs/cgroup/pids.max')\nif not p.exists(): p=Path('/sys/fs/cgroup/pids/pids.max')\nassert p.read_text().strip()=='64'\ntry:\n for i in range(512):\n  if os.fork()==0: time.sleep(60);os._exit(0)\nexcept OSError as e:\n assert e.errno==errno.EAGAIN\n raise SystemExit(2)\n"
			if cancellation {
				source = "import time; time.sleep(60)"
			}
			ws.AddFile("main.py", []byte(source), 0o644)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			type finished struct {
				result Result
				err    error
			}
			done := make(chan finished, 1)
			go func() {
				result, err := ws.Run(ctx, RunSpec{
					Image: python, Cmd: []string{"python3", "main.py"}, TimeLimit: 5 * time.Second, MemoryBytes: 128 * mb,
				})
				done <- finished{result, err}
			}()
			if cancellation {
				waitCtx, stop := context.WithTimeout(t.Context(), 10*time.Second)
				defer stop()
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
			waiting:
				for {
					containers, err := ws.sb.cli.ContainerList(waitCtx, client.ContainerListOptions{
						Filters: client.Filters{}.Add("volume", ws.volume).Add("label", labelKey+"=run"),
					})
					if err != nil {
						t.Fatal(err)
					}
					if len(containers.Items) == 1 {
						break waiting
					}
					select {
					case <-waitCtx.Done():
						t.Fatal("solution did not start")
					case <-ticker.C:
					}
				}
				cancel()
			}
			result := <-done
			if cancellation {
				if !errors.Is(result.err, context.Canceled) {
					t.Fatalf("cancel result: %v", result.err)
				}
			} else if result.err != nil || result.result.ExitCode != 2 {
				t.Fatalf("fork result: %+v err=%v", result.result, result.err)
			}
			containers, err := ws.sb.cli.ContainerList(t.Context(), client.ContainerListOptions{
				All: true, Filters: client.Filters{}.Add("volume", ws.volume),
			})
			if err != nil || len(containers.Items) != 1 || containers.Items[0].ID != ws.keeper {
				t.Fatalf("solution container survived: %+v err=%v", containers.Items, err)
			}
			top, err := ws.sb.cli.ContainerTop(t.Context(), ws.keeper, client.ContainerTopOptions{})
			if err != nil || len(top.Processes) != 2 {
				t.Fatalf("workspace keeper processes: %+v err=%v", top.Processes, err)
			}
		})
	}
}

func TestDockerOtherWorkspaceUnreadable(t *testing.T) {
	neighbor := newWorkspace(t, python)
	neighbor.AddFile("neighbor-secret", []byte("private-submission"), 0o600)
	if result := run(t, neighbor, RunSpec{
		Image: python, Cmd: []string{"python3", "-c", "from pathlib import Path; assert Path('neighbor-secret').read_text()=='private-submission'"},
		TimeLimit: 5 * time.Second, MemoryBytes: 64 * mb,
	}); result.ExitCode != 0 {
		t.Fatalf("neighbor setup: %q", result.Stderr)
	}
	attacker := newWorkspace(t, python)
	source := fmt.Sprintf("from pathlib import Path\nfor p in ['neighbor-secret', '/proc/1/root/work/box/neighbor-secret', %q]:\n try: Path(p).read_bytes()\n except (FileNotFoundError,PermissionError): pass\n else: raise AssertionError(p)\nprint('isolated')\n", "/var/lib/docker/volumes/"+neighbor.volume+"/_data/box/neighbor-secret")
	attacker.AddFile("main.py", []byte(source), 0o644)
	result := run(t, attacker, RunSpec{Image: python, Cmd: []string{"python3", "main.py"}, TimeLimit: 5 * time.Second, MemoryBytes: 64 * mb})
	if result.ExitCode != 0 || strings.TrimSpace(string(result.Stdout)) != "isolated" {
		t.Fatalf("neighbor files readable: exit=%d stderr=%q", result.ExitCode, result.Stderr)
	}
}
