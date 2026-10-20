// Package sandbox запускает недоверенный код студентов в изолированных Docker-контейнерах.
//
// Каждый запуск — отдельный контейнер:
//   - без сети;
//   - корневая ФС только на чтение;
//   - непривилегированный пользователь nobody (65534);
//   - все capabilities сняты, no-new-privileges;
//   - лимиты на память (без swap), CPU, число процессов, размер файлов и открытые файлы;
//   - ограничение по реальному времени и объёму вывода.
//
// Файлы решения лежат в volume рабочего пространства (Workspace), которое живёт одну
// проверку: компиляция и прогоны тестов видят один и тот же каталог /work/box.
// Для дополнительной изоляции ядра можно указать runtime gVisor (runsc).
package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

const (
	// WorkDir — рабочий каталог решения внутри контейнера.
	WorkDir = "/work/box"
	// nobody — пользователь, от имени которого выполняется код.
	nobody = "65534:65534"

	labelKey = "autocheck.sandbox"
)

// Значения по умолчанию для RunSpec.
const (
	DefaultPidsLimit     = 64
	DefaultOutputLimit   = 16 << 20 // 16 МБ на stdout и stderr вместе
	DefaultFileSizeLimit = 64 << 20 // 64 МБ на один файл
	DefaultCPUs          = 1.0
)

// Sandbox запускает контейнеры через Docker Engine API.
type Sandbox struct {
	cli     *client.Client
	runtime string
}

// New подключается к Docker по переменным окружения (DOCKER_HOST и т.п.).
// runtime — OCI runtime контейнеров, например "runsc" для gVisor; пустой — по умолчанию.
func New(runtime string) (*Sandbox, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return &Sandbox{cli: cli, runtime: runtime}, nil
}

// Close закрывает соединение с Docker.
func (s *Sandbox) Close() error {
	return s.cli.Close()
}

// EnsureImage скачивает образ, если его нет локально.
func (s *Sandbox) EnsureImage(ctx context.Context, image string) error {
	if _, err := s.cli.ImageInspect(ctx, image); err == nil {
		return nil
	}
	resp, err := s.cli.ImagePull(ctx, image, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %s: %w", image, err)
	}
	defer func() { _ = resp.Close() }()
	if err := resp.Wait(ctx); err != nil {
		return fmt.Errorf("pull image %s: %w", image, err)
	}
	return nil
}

// Workspace — рабочий каталог одной проверки (docker volume).
type Workspace struct {
	sb     *Sandbox
	volume string

	mu      sync.Mutex
	pending map[string]file
}

type file struct {
	data []byte
	mode int64
}

// NewWorkspace создаёт рабочее пространство. Его нужно закрыть через Close.
func (s *Sandbox) NewWorkspace(ctx context.Context) (*Workspace, error) {
	name := "autocheck-ws-" + uuid.NewString()
	if _, err := s.cli.VolumeCreate(ctx, client.VolumeCreateOptions{
		Name: name, Labels: map[string]string{labelKey: "workspace"},
	}); err != nil {
		return nil, fmt.Errorf("create workspace volume: %w", err)
	}
	return &Workspace{sb: s, volume: name, pending: map[string]file{}}, nil
}

// AddFile кладёт файл в WorkDir. Файлы копируются перед следующим запуском.
func (w *Workspace) AddFile(name string, data []byte, mode int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending[name] = file{data: data, mode: mode}
}

// Close удаляет рабочее пространство со всеми файлами.
func (w *Workspace) Close(ctx context.Context) error {
	if _, err := w.sb.cli.VolumeRemove(ctx, w.volume, client.VolumeRemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("remove workspace volume %s: %w", w.volume, err)
	}
	return nil
}

// RunSpec — что и с какими ограничениями запустить.
type RunSpec struct {
	Image string
	Cmd   []string
	Env   []string
	Stdin []byte

	// TimeLimit — ограничение реального времени; по его истечении процесс убивается.
	TimeLimit time.Duration
	// MemoryBytes — ограничение памяти (swap запрещён). 0 — без ограничения не допускается.
	MemoryBytes int64
	// CPUs — доля процессорного времени, по умолчанию 1 ядро.
	CPUs float64
	// PidsLimit — максимум процессов и потоков, по умолчанию DefaultPidsLimit.
	PidsLimit int64
	// OutputLimit — сколько байт stdout+stderr сохранить; при превышении процесс убивается.
	OutputLimit int64
	// FileSizeLimit — максимальный размер файла, который может создать процесс.
	FileSizeLimit int64
}

// Result — итог запуска.
type Result struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	WallTime time.Duration
	// TimedOut — процесс убит по TimeLimit.
	TimedOut bool
	// OOMKilled — процесс убит ядром за превышение памяти.
	OOMKilled bool
	// OutputLimitExceeded — вывод превысил OutputLimit, процесс убит.
	OutputLimitExceeded bool
}

// Run запускает команду в новом контейнере рабочего пространства и ждёт завершения.
// Ошибка возвращается только при сбое самой песочницы; поведение программы
// (код возврата, превышения) описывает Result.
func (w *Workspace) Run(ctx context.Context, spec RunSpec) (Result, error) {
	if spec.TimeLimit <= 0 || spec.MemoryBytes <= 0 {
		return Result{}, errors.New("sandbox: TimeLimit and MemoryBytes are required")
	}
	spec = withDefaults(spec)

	id, err := w.create(ctx, spec)
	if err != nil {
		return Result{}, err
	}
	// Контейнер удаляется в любом случае, даже если ctx уже отменён.
	defer func() {
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _ = w.sb.cli.ContainerRemove(rmCtx, id, client.ContainerRemoveOptions{Force: true})
	}()

	if err := w.copyPending(ctx, id); err != nil {
		return Result{}, err
	}
	return w.sb.run(ctx, id, spec)
}

func withDefaults(spec RunSpec) RunSpec {
	if spec.CPUs <= 0 {
		spec.CPUs = DefaultCPUs
	}
	if spec.PidsLimit <= 0 {
		spec.PidsLimit = DefaultPidsLimit
	}
	if spec.OutputLimit <= 0 {
		spec.OutputLimit = DefaultOutputLimit
	}
	if spec.FileSizeLimit <= 0 {
		spec.FileSizeLimit = DefaultFileSizeLimit
	}
	return spec
}

func (w *Workspace) create(ctx context.Context, spec RunSpec) (string, error) {
	init := true
	resp, err := w.sb.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:           spec.Image,
			Cmd:             spec.Cmd,
			Env:             append([]string{"HOME=" + WorkDir}, spec.Env...),
			User:            nobody,
			WorkingDir:      WorkDir,
			AttachStdin:     true,
			AttachStdout:    true,
			AttachStderr:    true,
			OpenStdin:       true,
			StdinOnce:       true,
			NetworkDisabled: true,
			Labels:          map[string]string{labelKey: "run"},
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    "none",
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			Runtime:        w.sb.runtime,
			// init (tini) собирает зомби и пробрасывает сигналы: программа не PID 1.
			Init:  &init,
			Tmpfs: map[string]string{"/tmp": "rw,nosuid,nodev,size=16m"},
			Mounts: []mount.Mount{{
				Type: mount.TypeVolume, Source: w.volume, Target: "/work",
			}},
			Resources: container.Resources{
				Memory:     spec.MemoryBytes,
				MemorySwap: spec.MemoryBytes, // swap запрещён: память = память+swap
				NanoCPUs:   int64(spec.CPUs * 1e9),
				PidsLimit:  &spec.PidsLimit,
				Ulimits: []*container.Ulimit{
					{Name: "nofile", Soft: 256, Hard: 256},
					{Name: "fsize", Soft: spec.FileSizeLimit, Hard: spec.FileSizeLimit},
					{Name: "core", Soft: 0, Hard: 0},
				},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	return resp.ID, nil
}

// copyPending копирует добавленные файлы в WorkDir. Владелец — nobody, чтобы
// код мог создавать рядом файлы (например, компилятор — бинарник).
func (w *Workspace) copyPending(ctx context.Context, containerID string) error {
	w.mu.Lock()
	files := w.pending
	w.pending = map[string]file{}
	w.mu.Unlock()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := func(h *tar.Header) error {
		h.Uid, h.Gid, h.ModTime = 65534, 65534, time.Now()
		return tw.WriteHeader(h)
	}
	// Каталог box создаётся при первом копировании; повторная запись заголовка безвредна.
	if err := hdr(&tar.Header{Name: "box/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		f := files[name]
		if err := hdr(&tar.Header{Name: "box/" + name, Mode: f.mode, Size: int64(len(f.data))}); err != nil {
			return err
		}
		if _, err := tw.Write(f.data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if _, err := w.sb.cli.CopyToContainer(ctx, containerID, client.CopyToContainerOptions{
		DestinationPath: "/work", Content: &buf, CopyUIDGID: true,
	}); err != nil {
		return fmt.Errorf("copy files to workspace: %w", err)
	}
	return nil
}

func (s *Sandbox) run(ctx context.Context, id string, spec RunSpec) (Result, error) {
	att, err := s.cli.ContainerAttach(ctx, id, client.ContainerAttachOptions{
		Stream: true, Stdin: true, Stdout: true, Stderr: true,
	})
	if err != nil {
		return Result{}, fmt.Errorf("attach: %w", err)
	}
	defer att.Close()

	// Ожидание подписывается до старта, чтобы не пропустить быстрое завершение.
	wait := s.cli.ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit})

	if _, err := s.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return Result{}, fmt.Errorf("start: %w", err)
	}
	started := time.Now()

	go func() {
		_, _ = att.Conn.Write(spec.Stdin)
		_ = att.CloseWrite()
	}()

	limit := newOutputLimit(spec.OutputLimit)
	out, errOut := &limitedBuffer{limit: limit}, &limitedBuffer{limit: limit}
	copied := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(out, errOut, att.Reader)
		copied <- err
	}()

	var res Result
	timer := time.NewTimer(spec.TimeLimit)
	defer timer.Stop()
	kill := func() {
		_, _ = s.cli.ContainerKill(context.WithoutCancel(ctx), id, client.ContainerKillOptions{})
	}

	select {
	case <-wait.Result:
	case err := <-wait.Error:
		kill()
		return Result{}, fmt.Errorf("wait: %w", err)
	case <-timer.C:
		res.TimedOut = true
		kill()
		<-wait.Result
	case <-limit.exceeded:
		res.OutputLimitExceeded = true
		kill()
		<-wait.Result
	case <-ctx.Done():
		kill()
		return Result{}, ctx.Err()
	}
	res.WallTime = time.Since(started)

	// Поток вывода закрывается после выхода процесса; дочитываем остаток.
	select {
	case <-copied:
	case <-time.After(5 * time.Second):
	}

	info, err := s.cli.ContainerInspect(context.WithoutCancel(ctx), id, client.ContainerInspectOptions{})
	if err != nil {
		return Result{}, fmt.Errorf("inspect: %w", err)
	}
	res.ExitCode = info.Container.State.ExitCode
	res.OOMKilled = info.Container.State.OOMKilled
	res.Stdout, res.Stderr = out.Bytes(), errOut.Bytes()
	return res, nil
}

// outputLimit — общий для stdout и stderr лимит вывода. При превышении закрывает
// exceeded (один раз); данные сверх лимита отбрасываются, поток не блокируется.
type outputLimit struct {
	mu       sync.Mutex
	left     int64
	exceeded chan struct{}
	once     sync.Once
}

func newOutputLimit(n int64) *outputLimit {
	return &outputLimit{left: n, exceeded: make(chan struct{})}
}

// take резервирует место под n байт и возвращает, сколько из них можно сохранить.
func (l *outputLimit) take(n int) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	keep := int(min(int64(n), max(l.left, 0)))
	l.left -= int64(n)
	if l.left < 0 {
		l.once.Do(func() { close(l.exceeded) })
	}
	return keep
}

// limitedBuffer — буфер одного потока вывода с общим лимитом.
type limitedBuffer struct {
	limit *outputLimit
	mu    sync.Mutex
	buf   bytes.Buffer
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if keep := b.limit.take(len(p)); keep > 0 {
		b.mu.Lock()
		b.buf.Write(p[:keep])
		b.mu.Unlock()
	}
	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

var _ io.Writer = (*limitedBuffer)(nil)
