//go:build linux && amd64

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/guestbuild"
	"github.com/clawkwork/clawk/internal/vsockclient"
	"github.com/clawkwork/clawk/machine"
	"github.com/clawkwork/clawk/machine/kernel"
	"github.com/clawkwork/clawk/machine/oci"
	qemubackend "github.com/clawkwork/clawk/machine/qemu"
	"golang.org/x/sys/unix"
)

type QEMUProvider struct {
	store    *config.Store
	progress Progress
}

func NewQEMUProvider(store *config.Store) *QEMUProvider { return &QEMUProvider{store: store} }
func (q *QEMUProvider) SetProgress(p Progress)          { q.progress = p }
func (q *QEMUProvider) vmDir(sb *config.Sandbox) string { return q.store.VMDir(sb.Key()) }
func (q *QEMUProvider) QEMUStateDir(sb *config.Sandbox) string {
	return filepath.Join(q.vmDir(sb), "qemu")
}
func (q *QEMUProvider) GuestWorkspaceRoot() string { return WorkspaceRoot }

// Keep the operation lock outside VMDir: destroy removes VMDir, and unlinking
// an active lock would let another process acquire a different inode's lock.
func (q *QEMUProvider) operationLock(sb *config.Sandbox) (func(), error) {
	path := q.vmDir(sb) + ".qemu.lock"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another operation is in progress for %s: %w", sb.Key(), err)
	}
	return func() { _ = f.Close() }, nil
}

// Version 4 changes normal cold boots to disposable rootfs clones, as on VZ.
// Keep this migration fence so old persistent disks are never silently erased.
const qemuGuestVersion = "4\n"

func CheckQEMUGuest(vmDir string) error {
	if _, err := os.Stat(filepath.Join(vmDir, "disk.raw")); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(vmDir, "qemu-guest-version"))
	if err != nil || string(b) != qemuGuestVersion {
		return errors.New("QEMU guest disk predates disposable cold-boot rootfs support; save guest-only files, then recreate the sandbox with clawk destroy and its original creation command")
	}
	return nil
}

func (q *QEMUProvider) Create(sb *config.Sandbox) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	return q.CreateContext(ctx, sb)
}

func (q *QEMUProvider) CreateContext(ctx context.Context, sb *config.Sandbox) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	unlock, err := q.operationLock(sb)
	if err != nil {
		return err
	}
	defer unlock()
	if err := CheckQEMUGuest(q.vmDir(sb)); err != nil {
		return err
	}
	if pid := readPIDFile(filepath.Join(q.vmDir(sb), "qemu-daemon.pid")); pid > 0 && qemuDaemonAlive(pid, sb.Key()) {
		return nil
	}
	progress := q.progress
	if progress == nil {
		progress = PlainProgress{}
	}
	defer progress.Close()
	ctxProbe, stopProbe := context.WithTimeout(ctx, 15*time.Second)
	errProbe := qemubackend.Preflight(ctxProbe)
	stopProbe()
	if errProbe != nil {
		return errProbe
	}

	if sb.Image == "" {
		return fmt.Errorf("sandbox %q has no OCI image configured", sb.Name)
	}
	if sb.NestedVirt && sb.Kernel == "" {
		return errors.New("QEMU nested virtualization requires a KVM-enabled guest kernel; set vm ( kernel <path-or-url> ) using the qemu.fragment recipe in docs/qemu.md")
	}
	if len(sb.Phases) == 0 || sb.Phases[0].Worktree == "" {
		return fmt.Errorf("sandbox %q has no phase with a Worktree", sb.Name)
	}
	vmDir := q.vmDir(sb)
	if err := os.MkdirAll(vmDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(vmDir, 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, qemubackend.DefaultStartupBudgets().Kernel)
	defer cancel()
	progress.Step("Preparing guest binaries")
	bins, err := guestbuild.Build(ctx, q.store.CacheDir(), qemubackend.GuestArch())
	if err != nil {
		return fmt.Errorf("guest binaries: %w", err)
	}
	if bins.Cached {
		progress.Skip()
	} else {
		progress.StepDone("Built guest binaries (clawk-init, pty-agent)")
	}
	progress.Step("Preparing QEMU kernel")
	kernelPath, err := kernel.FetchQEMU(ctx, q.store.CacheDir(), qemubackend.GuestArch(), sb.Kernel)
	if err != nil {
		return fmt.Errorf("kernel: %w", err)
	}
	progress.StepDone("QEMU kernel ready")
	if err := qemubackend.ValidateKernel(kernelPath); err != nil {
		return err
	}
	if !machine.SuspendStateExists(filepath.Join(vmDir, "suspend")) {
		if err := SeedClaudeStateDir(q.store.StateDir(sb.Key()), q.store.RootDir()); err != nil {
			return err
		}
		if err := SeedClaudeMCP(q.store.StateDir(sb.Key()), sb.MCP); err != nil {
			return err
		}
		if err := SeedClaudeMemory(q.store.StateDir(sb.Key()), sb.Memory); err != nil {
			return err
		}
	}
	progress.Step("Preparing rootfs from %s", sb.Image)
	rootfs := OCIRootFS(sb, q.store.CacheDir(), bins)
	rootfs.Platform = "linux/" + qemubackend.GuestArch()
	opts := oci.OptionsForImage(rootfs)
	opts.Progress = func(u oci.ProgressUpdate) {
		switch u.Phase {
		case oci.PhaseStart:
			progress.Step("Building rootfs from %s (first build)", sb.Image)
		case oci.PhaseDownload:
			progress.SetBars(layerBars(u.Downloads))
			progress.Detail("downloading %d layers", len(u.Downloads))
		case oci.PhaseUnpack:
			progress.SetBars(nil)
			if u.Layers > 0 {
				progress.SetFraction(float64(u.Layer) / float64(u.Layers))
				progress.Detail("unpacking layer %d/%d · %d MiB", u.Layer, u.Layers, u.UnpackedBytes>>20)
			}
		}
	}
	res, err := oci.Build(ctx, opts)
	if err != nil {
		return fmt.Errorf("rootfs: %w", err)
	}
	if res.UnpackedBytes > 0 {
		progress.StepDone("Built rootfs from %s (%d MiB unpacked)", sb.Image, res.UnpackedBytes>>20)
	} else {
		progress.StepDone("Rootfs %s (cached)", sb.Image)
	}
	if err = os.WriteFile(filepath.Join(vmDir, "qemu-guest-version"), []byte(qemuGuestVersion), 0o600); err != nil {
		return err
	}
	sb.GuestIP = guestIP
	return nil
}

func (q *QEMUProvider) Start(sb *config.Sandbox) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	return q.StartContext(ctx, sb)
}

func (q *QEMUProvider) StartContext(ctx context.Context, sb *config.Sandbox) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	unlock, err := q.operationLock(sb)
	if err != nil {
		return err
	}
	defer unlock()
	if err := CheckQEMUGuest(q.vmDir(sb)); err != nil {
		return err
	}
	progress := q.progress
	if progress == nil {
		progress = PlainProgress{}
	}
	defer progress.Close()
	pidPath := filepath.Join(q.vmDir(sb), "qemu-daemon.pid")
	if pid := readPIDFile(pidPath); pid > 0 && qemuDaemonAlive(pid, sb.Key()) {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "__qemud", sb.Key())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("spawning qemud: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait(); close(exited) }()
	started := time.Now()
	progress.Step("Booting QEMU VM")
	ctx, cancel := context.WithTimeout(ctx, qemubackend.DefaultStartupBudgets().Total(machine.SuspendStateExists(filepath.Join(q.vmDir(sb), "suspend"))))
	defer cancel()
	err = waitQEMUStartup(ctx, exited, func(ctx context.Context) error {
		ping, done := context.WithTimeout(ctx, 500*time.Millisecond)
		defer done()
		return vsockclient.Ping(ping, filepath.Join(q.vmDir(sb), "agent.sock"), 0, 400*time.Millisecond)
	})
	if err != nil {
		// The child may fail before writing its pidfile. Signal the process we
		// spawned as well as stopping a daemon recorded in the store.
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = q.stop(sb)
		select {
		case <-exited:
		case <-time.After(qemubackend.DefaultStartupBudgets().Cleanup):
			_ = cmd.Process.Kill()
		}
		return fmt.Errorf("agent did not become ready: %w%s%s", err, LogTail(filepath.Join(q.vmDir(sb), "qemud.log"), 15, "clawk qemu daemon log"), ConsoleTail(filepath.Join(q.vmDir(sb), "console.log"), 20))
	}

	progress.StepDone("Ready in %.1fs", time.Since(started).Seconds())
	return nil
}

// Keep exit detection independent of the readiness probe's network timeout.
func waitQEMUStartup(ctx context.Context, exited <-chan error, ready func(context.Context) error) error {
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		for {
			if err := ready(probeCtx); err == nil {
				result <- nil
				return
			}
			select {
			case <-probeCtx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
	select {
	case err := <-exited:
		return fmt.Errorf("qemu daemon exited during startup: %v", err)
	case <-ctx.Done():
		return ctx.Err()
	case <-result:
		return nil
	}
}

func (q *QEMUProvider) Stop(sb *config.Sandbox) error {
	unlock, err := q.operationLock(sb)
	if err != nil {
		return err
	}
	defer unlock()
	return q.stop(sb)
}
func (q *QEMUProvider) stop(sb *config.Sandbox) error {
	pid := readPIDFile(filepath.Join(q.vmDir(sb), "qemu-daemon.pid"))
	if pid < 1 || !qemuDaemonAlive(pid, sb.Key()) {
		return nil
	}
	return stopByPIDFile(filepath.Join(q.vmDir(sb), "qemu-daemon.pid"), 20*time.Second)
}
func (q *QEMUProvider) Destroy(sb *config.Sandbox) error {
	unlock, err := q.operationLock(sb)
	if err != nil {
		return err
	}
	defer unlock()
	if err := q.stop(sb); err != nil {
		return err
	}
	return os.RemoveAll(q.vmDir(sb))
}
func (q *QEMUProvider) Status(sb *config.Sandbox) (string, error) {
	if p := readPIDFile(filepath.Join(q.vmDir(sb), "qemu-daemon.pid")); p > 0 && qemuDaemonAlive(p, sb.Key()) {
		return "running", nil
	}
	return "stopped", nil
}
func (q *QEMUProvider) Shell(sb *config.Sandbox, workdir string) error {
	return agentShell(q.vmDir(sb), workdir)
}
func (q *QEMUProvider) Exec(sb *config.Sandbox, command ...string) error {
	return agentExec(q.vmDir(sb), GuestUser, command...)
}
func (q *QEMUProvider) ExecCapture(sb *config.Sandbox, command ...string) (string, error) {
	return agentOutput(q.vmDir(sb), GuestUser, command...)
}
func (q *QEMUProvider) ExecRoot(sb *config.Sandbox, command ...string) error {
	return agentExec(q.vmDir(sb), "root", command...)
}

func qemuDaemonAlive(pid int, key string) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	args := strings.Split(string(b), "\x00")
	return len(args) >= 3 && args[1] == "__qemud" && args[2] == key && processAlive(pid)
}
