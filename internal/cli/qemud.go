//go:build linux && amd64

package cli

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/clawkwork/clawk/internal/agentembed"
	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/debug"
	"github.com/clawkwork/clawk/internal/guestbuild"
	"github.com/clawkwork/clawk/internal/guestcfg"
	"github.com/clawkwork/clawk/internal/netfilter"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/clawkwork/clawk/internal/vsockclient"
	"github.com/clawkwork/clawk/internal/vzdctl"
	"github.com/clawkwork/clawk/machine"
	"github.com/clawkwork/clawk/machine/kernel"
	"github.com/clawkwork/clawk/machine/qemu"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

// qemuDaemonMachine is the backend contract needed by the daemon. Check it
// once so an incomplete backend produces an initialization error, not a panic.
type qemuDaemonMachine interface {
	machine.Machine
	SetVSockAuthToken(string) error
	HostVSockPort(uint32) uint32
	SetPrepareBoot(func() error)
	SetGuestLifecycle(func(context.Context) error, func(context.Context) error)
	Close() error
}

func init() { rootCmd.AddCommand(qemudCmd) }

var qemudCmd = &cobra.Command{Use: "__qemud <sandbox>", Hidden: true, Args: cobra.ExactArgs(1), RunE: runQemud}

func runQemud(_ *cobra.Command, args []string) (retErr error) {
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()
	sb, err := store.Load(args[0])
	if err != nil {
		return err
	}
	vmDir := store.VMDir(sb.Key())
	if err = os.MkdirAll(vmDir, 0o700); err != nil {
		return err
	}
	if err = os.Chmod(vmDir, 0o700); err != nil {
		return err
	}
	lf, err := os.OpenFile(filepath.Join(vmDir, "qemud.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	logger := log.New(lf, "", log.LstdFlags)
	defer func() {
		if retErr != nil {
			logger.Printf("FATAL: %v", retErr)
		}
	}()
	logger.Printf("qemud starting pid=%d sandbox=%q debug=%v", os.Getpid(), sb.Name, debug.Enabled())
	lock, err := os.OpenFile(filepath.Join(vmDir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("sandbox daemon already running: %w", err)
	}
	pidPath := filepath.Join(vmDir, "qemu-daemon.pid")
	if err = os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return err
	}
	defer os.Remove(pidPath)
	if err := sandbox.CheckQEMUGuest(vmDir); err != nil {
		return err
	}
	if err := prepareQEMUSuspendState(vmDir, sb.Image, logger); err != nil {
		return err
	}
	authToken, err := qemuGuestToken(vmDir)
	if err != nil {
		return err
	}
	allow, err := startAllowList(sb, logger)
	if err != nil {
		return err
	}
	defer allow.Stop()
	rev := newReverseProxy(logger)
	rev.Set(sb.ReverseForwards)
	ser := newSerialProxy(logger)
	ser.Set(sb.Serials)
	lc := newVMLifecycle(sb.Key(), vmDir, logger)
	ctl, err := vzdctl.Start(vzdctl.SocketPath(vmDir), controlHandlers(sb, allow, lc, rev, ser, logger))
	if err == nil {
		defer ctl.Close()
	} else {
		logger.Printf("control socket disabled: %v", err)
	}
	// Keep guest services alive until graceful shutdown finishes. Startup
	// uses signalCtx so cancellation still interrupts preparation/readiness.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov := sandbox.NewQEMUProvider(store)
	spec, err := buildQEMUSpec(signalCtx, sb, vmDir, allow, logger)
	if err != nil {
		return fmt.Errorf("building spec: %w", err)
	}
	backend, err := machine.Get("qemu")
	if err != nil {
		return err
	}
	raw, err := backend.New(signalCtx, spec, prov.QEMUStateDir(sb))
	if err != nil {
		return err
	}
	m, ok := raw.(qemuDaemonMachine)
	if !ok {
		stop, done := context.WithTimeout(context.Background(), qemu.DefaultStartupBudgets().Cleanup)
		defer done()
		_ = raw.Stop(stop, false)
		if closer, ok := raw.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
		return fmt.Errorf("initializing QEMU daemon: backend %T lacks required guest-service capabilities", raw)
	}
	defer func() {
		cancel()
		stop, done := context.WithTimeout(context.Background(), qemu.DefaultStartupBudgets().Cleanup)
		defer done()
		_ = m.Stop(stop, false)
		_ = m.Close()
	}()
	if err := m.SetVSockAuthToken(authToken); err != nil {
		return err
	}
	// The backend reserves a host-unique CID. Mirror its listener mapping in
	// the guest manifest before boot, never in an already-suspended config disk.
	prepareBoot := func() error {
		manifest, err := sandbox.QEMUGuestManifest(sb, store)
		if err != nil {
			return err
		}
		for i := range manifest.Services {
			manifest.Services[i].Env = append(manifest.Services[i].Env, fmt.Sprintf("CLAWK_HOST_PORT_OFFSET=%d", m.HostVSockPort(0)), "CLAWK_VSOCK_AUTH="+authToken)
		}
		return guestcfg.WriteDisk(manifest, filepath.Join(vmDir, sandbox.OCIConfigDiskName))
	}
	m.SetPrepareBoot(prepareBoot)
	if !hasSuspendState(suspendStateDir(vmDir)) {
		if err := prepareBoot(); err != nil {
			return err
		}
	}

	prepareCtx, prepareCancel := context.WithTimeout(signalCtx, qemu.DefaultStartupBudgets().Preparation)
	err = m.Create(prepareCtx)
	prepareCancel()
	if err != nil {
		return err
	}
	meta := machine.SuspendMeta{Backend: "qemu", SpecFingerprint: machine.SuspendSpecFingerprint(spec), ClawkVersion: buildVersion(), GuestSources: agentembed.SourcesHash(), Image: sb.Image}
	lc.setSuspendMeta(meta)
	restored, err := restoreOrStart(signalCtx, m, vmDir, logger, meta)
	if err != nil {
		return err
	}
	readyCtx, readyCancel := context.WithTimeout(signalCtx, qemu.DefaultStartupBudgets().GuestReady)
	defer readyCancel()
	services, err := startQEMUGuestServices(ctx, readyCtx, m, sb, vmDir, rev, ser, logger)
	if err != nil {
		return err
	}
	defer services.Close()
	initializing := services.admin.sockPath
	m.SetGuestLifecycle(
		func(c context.Context) error {
			_, code, e := vsockclient.Output(c, initializing, 0, "root", "/bin/sh", "-c", "sync")
			if e != nil {
				return e
			}
			if code != 0 {
				return fmt.Errorf("guest sync exited %d", code)
			}
			return nil
		},
		func(c context.Context) error {
			_, _, e := vsockclient.Output(c, initializing, 0, "root", "/bin/sh", "-c", "kill -TERM 1")
			return e
		})
	lc.attach(m, restored)
	if err := services.Publish(ctx, m, sb, vmDir, lc, logger); err != nil {
		return err
	}
	startQEMUBalloon(ctx, m, spec, logger)
	logger.Printf("vm ready: qemu %s shares=%d restored=%v", qemu.GuestArch(), len(spec.Shares), restored)
	return waitAndShutdown(signalCtx, m, logger)
}

func buildQEMUSpec(ctx context.Context, sb *config.Sandbox, vmDir string, allow *netfilter.AllowList, logger *log.Logger) (machine.Spec, error) {
	if sb.NestedVirt && sb.Kernel == "" {
		return machine.Spec{}, fmt.Errorf("QEMU nested virtualization requires a custom KVM-enabled guest kernel; set vm ( kernel <path-or-url> ); see docs/qemu.md")
	}
	ctx, cancel := context.WithTimeout(ctx, qemu.DefaultStartupBudgets().Kernel)
	defer cancel()
	vmlinux, err := kernel.FetchQEMU(ctx, store.CacheDir(), qemu.GuestArch(), sb.Kernel)
	if err != nil {
		return machine.Spec{}, fmt.Errorf("kernel: %w", err)
	}
	swapPath, err := sandbox.EnsureSwapDisk(vmDir, sandbox.SwapDiskMiB(sb))
	if err != nil {
		return machine.Spec{}, err
	}
	spec, err := buildQEMUBaseSpec(sb, vmDir, allow)
	if err != nil {
		return machine.Spec{}, err
	}
	qemuCmdline := strings.Replace(sandbox.OCICmdline, "console=hvc0", "console="+qemu.Console(), 1)
	// Normal boots clone the current image and injected guest binaries. Only
	// restoration reuses the disk paired with saved memory, just as VZ does.
	bins, err := guestbuild.Build(ctx, store.CacheDir(), qemu.GuestArch())
	if err != nil {
		return machine.Spec{}, err
	}
	var source machine.RootFS = sandbox.OCIRootFS(sb, store.CacheDir(), bins)
	if saved, ok := suspendBootRootFS(vmDir, "disk.raw", logger); ok {
		source = saved
	}
	rootfs, err := qemu.MaterializeRootFS(ctx, source, filepath.Join(vmDir, "disk.raw"))
	if err != nil {
		return machine.Spec{}, fmt.Errorf("preparing guest rootfs: %w", err)
	}
	spec.Boot = machine.DirectKernel{Vmlinux: vmlinux, Cmdline: qemuCmdline}
	spec.RootFS = rootfs
	spec.Disks = []machine.Disk{{Path: filepath.Join(vmDir, sandbox.OCIConfigDiskName), ReadOnly: true}}
	if swapPath != "" {
		spec.Disks = append(spec.Disks, machine.Disk{Path: swapPath})
	}
	return spec, nil
}

// Reject old guest code before preparing any disks. Otherwise a successful
// restore can silently keep running an obsolete init after a host upgrade.
func prepareQEMUSuspendState(vmDir, image string, logger *log.Logger) error {
	dir := suspendStateDir(vmDir)
	if !hasSuspendState(dir) {
		return nil
	}
	saved, _ := machine.ReadSuspendMeta(dir)
	want := machine.SuspendMeta{GuestSources: agentembed.SourcesHash(), Image: image}
	if reason := saved.IncompatibleWith(want); reason != "" {
		logger.Printf("discarding suspended memory: %s; processes in the snapshot are lost", reason)
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("discarding incompatible guest state: %w", err)
		}
	}
	return nil
}
