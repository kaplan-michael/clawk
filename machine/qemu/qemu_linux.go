//go:build linux && amd64

package qemu

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/clawkwork/clawk/machine"
	"github.com/clawkwork/clawk/machine/internal/usermode"
)

func init() { machine.Register(backend{}) }

type backend struct{}

func (backend) Name() string { return Name }
func (backend) Capabilities() machine.Caps {
	return machine.Caps{OCIRootFS: true, DirectKernel: true, VSock: true,
		VirtioFS: true, UserModeNet: true, TAPNet: true, Snapshot: true, NestedVirt: nestedSupported()}
}

func (backend) New(ctx context.Context, spec machine.Spec, stateDir string) (machine.Machine, error) {
	if os.Geteuid() == 0 {
		return nil, errors.New("qemu: refusing to run as root; run clawk as an unprivileged user")
	}
	if !kvmUsable() {
		return nil, errors.New("qemu: KVM is required; /dev/kvm must be readable and writable by the invoking user")
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	bin, err := exec.LookPath(BinaryName())
	if err != nil {
		return nil, fmt.Errorf("qemu: %s not on PATH", BinaryName())
	}
	version, err := probeOutput(ctx, bin, "--version")
	if err != nil {
		return nil, err
	}
	if err := checkQEMUVersion(version); err != nil {
		return nil, fmt.Errorf("qemu: %w", err)
	}
	if err := checkSpec(spec); err != nil {
		return nil, err
	}
	spec.Shares = append([]machine.Share(nil), spec.Shares...)
	for i := range spec.Shares {
		path, err := filepath.Abs(spec.Shares[i].HostPath)
		if err == nil {
			path, err = filepath.EvalSymlinks(path)
		}
		if err != nil {
			return nil, fmt.Errorf("qemu: share %s: %w", spec.Shares[i].Tag, err)
		}
		spec.Shares[i].HostPath = path
	}
	if len(spec.Shares) > 0 {
		if _, err := FindVirtiofsd(); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("qemu: state dir: %w", err)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("qemu: secure state dir: %w", err)
	}
	autoCID := spec.VSockCID == 0
	if spec.VSockCID == 0 {
		cid, err := persistentCID(stateDir, spec.ID)
		if err != nil {
			return nil, err
		}
		spec.VSockCID = cid
	}
	v := &vm{spec: spec, stateDir: stateDir, binary: bin}
	if err := v.reserve(autoCID); err != nil {
		return nil, err
	}
	v.audio = audioBackend(ctx, bin)
	return v, nil
}

func persistentCID(stateDir, id string) (uint32, error) {
	lock, err := os.OpenFile(filepath.Join(stateDir, "vsock.cid.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return 0, fmt.Errorf("qemu: CID lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return 0, fmt.Errorf("qemu: lock CID allocation: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	p := filepath.Join(stateDir, "vsock.cid")
	if b, err := os.ReadFile(p); err == nil {
		n, e := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 32)
		if e == nil && n >= 3 && n <= 1048574 {
			return uint32(n), nil
		}
		return 0, fmt.Errorf("qemu: invalid persisted vsock CID in %s", p)
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("qemu: reading persisted vsock CID: %w", err)
	}
	// Stable allocation avoids CID churn across daemon restarts. Keep CIDs in a
	// bounded range so host-port namespacing can safely derive an offset later.
	cid := uint32(3) + crc32.ChecksumIEEE([]byte(id))%60000
	if err := os.WriteFile(p, []byte(strconv.FormatUint(uint64(cid), 10)+"\n"), 0o600); err != nil {
		return 0, err
	}
	return cid, nil
}

func checkSpec(spec machine.Spec) error {
	if spec.NestedVirt && !nestedSupported() {
		return fmt.Errorf("%w: nested virtualization is unavailable on this host", machine.ErrUnsupportedSpec)
	}
	if _, ok := spec.Boot.(machine.DirectKernel); !ok {
		return fmt.Errorf("%w: qemu only supports DirectKernel boot", machine.ErrUnsupportedSpec)
	}
	switch spec.RootFS.(type) {
	case machine.RawDisk, machine.OCIImage:
	default:
		return fmt.Errorf("%w: unsupported RootFS %T", machine.ErrUnsupportedSpec, spec.RootFS)
	}
	if spec.VSockCID > 1048574 {
		return fmt.Errorf("%w: CID too large for host port allocation", machine.ErrUnsupportedSpec)
	}
	if len(spec.Net) > 1 {
		return fmt.Errorf("%w: qemu currently supports at most one network interface", machine.ErrUnsupportedSpec)
	}
	for _, n := range spec.Net {
		switch n.(type) {
		case machine.UserMode:
		case machine.TAP:
		default:
			return fmt.Errorf("%w: unsupported Net %T", machine.ErrUnsupportedSpec, n)
		}
	}
	seenTags := make(map[string]struct{}, len(spec.Shares))
	for _, sh := range spec.Shares {
		if sh.Tag == "" || sh.HostPath == "" {
			return fmt.Errorf("%w: virtio-fs share requires tag and host path", machine.ErrUnsupportedSpec)
		}
		if strings.ContainsAny(sh.Tag, ",=") {
			return fmt.Errorf("%w: invalid virtio-fs tag %q", machine.ErrUnsupportedSpec, sh.Tag)
		}
		if _, exists := seenTags[sh.Tag]; exists {
			return fmt.Errorf("%w: duplicate virtio-fs tag %q", machine.ErrUnsupportedSpec, sh.Tag)
		}
		seenTags[sh.Tag] = struct{}{}
	}
	return nil
}

type vm struct {
	prepareBoot      func() error
	syncGuest        func(context.Context) error
	shutdownGuest    func(context.Context) error
	spec             machine.Spec
	stateDir, binary string
	mu               sync.Mutex
	opMu             contextMutex
	state            machine.State
	process          *child
	stack            *usermode.Stack
	cancel           context.CancelFunc
	virtiofs         []*child
	qmp              *qmpClient
	vhost            *os.File
	lock             *os.File
	listeners        []net.Listener
	failure          error
	incoming         bool
	audio            string
	authToken        string
}

func (v *vm) Create(ctx context.Context) error {
	if err := v.opMu.LockContext(ctx); err != nil {
		return err
	}
	defer v.opMu.Unlock()
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state == machine.StateCreated {
		return nil
	}
	if v.state == machine.StateRunning || v.state == machine.StatePaused {
		return fmt.Errorf("%w: create while active", machine.ErrInvalidState)
	}
	if v.state == machine.StateDestroyed {
		return fmt.Errorf("%w: create after destroy", machine.ErrInvalidState)
	}
	var disk machine.RawDisk
	if raw, ok := v.spec.RootFS.(machine.RawDisk); ok {
		if _, err := os.Stat(raw.Path); err != nil {
			return fmt.Errorf("qemu: rootfs %q: %w", raw.Path, err)
		}
		disk = raw
	} else {
		var err error
		disk, err = MaterializeRootFS(ctx, v.spec.RootFS, filepath.Join(v.stateDir, "disk.raw"))
		if err != nil {
			return fmt.Errorf("qemu: materializing rootfs: %w", err)
		}
	}
	dk := v.spec.Boot.(machine.DirectKernel)
	for _, p := range append([]string{disk.Path, dk.Vmlinux}, diskPaths(v.spec.Disks)...) {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("qemu: required file %q: %w", p, err)
		}
	}
	if err := ValidateKernel(dk.Vmlinux); err != nil {
		return err
	}
	for _, sh := range v.spec.Shares {
		st, err := os.Stat(sh.HostPath)
		if err != nil {
			return fmt.Errorf("qemu: share %q: %w", sh.HostPath, err)
		}
		if !st.IsDir() {
			return fmt.Errorf("qemu: share %q is not a directory", sh.HostPath)
		}
	}
	if !kvmUsable() {
		return errors.New("qemu: KVM became unavailable; /dev/kvm must be readable and writable")
	}
	v.spec.RootFS = disk
	v.state = machine.StateCreated
	return nil
}

// ValidateKernel checks the x86 boot header required by QEMU direct boot.
func ValidateKernel(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("qemu: kernel %q: %w", path, err)
	}
	defer f.Close()
	header := make([]byte, 0x206)
	if _, err := io.ReadFull(f, header); err != nil {
		return fmt.Errorf("qemu: kernel %q is too small for the x86 boot protocol", path)
	}
	if string(header[0x202:0x206]) != "HdrS" {
		return fmt.Errorf("qemu: kernel %q is not an x86 bzImage/vmlinuz (missing boot protocol HdrS signature); QEMU cannot direct-boot an ELF vmlinux", path)
	}
	return nil
}

func kvmUsable() bool {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	return f.Close() == nil
}

func diskPaths(ds []machine.Disk) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Path)
	}
	return out
}

func (v *vm) Start(ctx context.Context) error {
	if err := v.opMu.LockContext(ctx); err != nil {
		return err
	}
	defer v.opMu.Unlock()
	return v.start(ctx)
}
func (v *vm) start(ctx context.Context) (retErr error) {
	v.mu.Lock()
	state := v.state
	process, failure := v.process, v.failure
	v.mu.Unlock()
	if state == machine.StateRunning {
		healthy := process != nil && !process.exited() && failure == nil
		for _, helper := range v.virtiofs {
			healthy = healthy && !helper.exited()
		}
		if healthy {
			return nil
		}
		state = machine.StateStopped
	}
	if state != machine.StateCreated && state != machine.StateStopped {
		return fmt.Errorf("%w: start in %s", machine.ErrInvalidState, state)
	}
	if state == machine.StateStopped {
		// An observed child failure does not imply its siblings or vhost
		// descriptors have exited. Reap the entire previous instance before
		// reserving its CID and opening new helper sockets.
		v.cleanup()
		v.mu.Lock()
		v.state = machine.StateStopped
		v.mu.Unlock()
	}
	defer func() {
		if retErr != nil {
			v.cleanup()
			v.mu.Lock()
			v.state = machine.StateStopped
			v.mu.Unlock()
		}
	}()
	if v.vhost == nil {
		if err := v.reserveCID(false); err != nil {
			return err
		}
	}
	if !v.incoming && v.prepareBoot != nil {
		if err := v.prepareBoot(); err != nil {
			return err
		}
	}
	if err := v.startHelpers(ctx); err != nil {
		return err
	}
	args, err := v.buildArgs()
	if err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(v.stateDir, "qmp.sock"))
	logPath := v.spec.Serial.LogPath
	if logPath == "" {
		logPath = filepath.Join(v.stateDir, "console.log")
	}
	cmd := exec.Command(v.binary, args...)
	if v.vhost != nil {
		cmd.ExtraFiles = []*os.File{v.vhost}
	}
	p, err := launch(cmd, logPath)
	if err != nil {
		return fmt.Errorf("qemu start: %w", err)
	}
	v.mu.Lock()
	v.process = p
	v.failure = nil
	v.mu.Unlock()
	if err = os.WriteFile(filepath.Join(v.stateDir, "qemu.pid"), []byte(strconv.Itoa(p.cmd.Process.Pid)), 0o600); err != nil {
		return err
	}
	quoted := []string{strconv.Quote(v.binary)}
	for _, a := range args {
		quoted = append(quoted, strconv.Quote(a))
	}
	_ = os.WriteFile(filepath.Join(v.stateDir, "qemu.cmdline"), []byte(strings.Join(quoted, " ")+"\n"), 0o600)
	readyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	path := filepath.Join(v.stateDir, "qmp.sock")
	if err = waitPath(readyCtx, path, p); err != nil {
		return fmt.Errorf("qemu QMP readiness: %w", err)
	}
	if err = os.Chmod(path, 0o600); err != nil {
		return err
	}
	q, err := dialQMP(readyCtx, path)
	if err != nil {
		return err
	}
	v.mu.Lock()
	v.qmp = q
	v.state = machine.StateRunning
	v.mu.Unlock()
	return nil
}

func (v *vm) startHelpers(ctx context.Context) error {
	for _, n := range v.spec.Net {
		if um, ok := n.(machine.UserMode); ok {
			s, err := usermode.Start(usermode.Config{SockPath: filepath.Join(v.stateDir, "gvproxy.sock"), Forwards: um.Forwards, Filter: um.Filter, ID: v.stateDir})
			if err != nil {
				return err
			}
			_ = s.VMSocket.Close()
			_ = os.Remove(s.VMPath)
			serveCtx, cancel := context.WithCancel(context.Background())
			v.stack = s
			v.cancel = cancel
			go func() {
				if err := s.Serve(serveCtx); err != nil && serveCtx.Err() == nil {
					v.mu.Lock()
					if serveCtx.Err() == nil {
						v.failure = fmt.Errorf("gvproxy: %w", err)
					}
					v.mu.Unlock()
				}
			}()
		}
	}
	binary, err := FindVirtiofsd()
	if len(v.spec.Shares) > 0 && err != nil {
		return err
	}
	for i, sh := range v.spec.Shares {
		sock := filepath.Join(v.stateDir, fmt.Sprintf("virtiofs-%d.sock", i))
		_ = os.Remove(sock)
		args := virtiofsArgs(sh.HostPath, sock, sh.ReadOnly)
		p, err := launch(exec.Command(binary, args...), filepath.Join(v.stateDir, fmt.Sprintf("virtiofs-%d.log", i)))
		if err != nil {
			return err
		}
		v.virtiofs = append(v.virtiofs, p)
		ready, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = waitPath(ready, sock, p)
		cancel()
		if err != nil {
			return fmt.Errorf("virtiofsd share %q: %w", sh.Tag, err)
		}
	}
	return nil
}

func (v *vm) buildArgs() ([]string, error) {
	const limit = 8
	count := 1 + len(v.spec.Disks) + len(v.spec.Net) + 3
	pciSlots := 31
	if v.audio != "" {
		pciSlots-- // HDA consumes one PCI/MSI slot.
	}
	if len(v.spec.Shares) > pciSlots {
		return nil, fmt.Errorf("qemu: %d shares exceed the %d available PCI slots", len(v.spec.Shares), pciSlots)
	}
	if count > limit {
		return nil, fmt.Errorf("qemu: %d virtio devices exceed %d-device capacity; reduce disks or network devices", count, limit)
	}

	dk := v.spec.Boot.(machine.DirectKernel)
	root := v.spec.RootFS.(machine.RawDisk)
	mem := v.spec.MemoryMiB
	if v.spec.MemoryMaxMiB > mem {
		mem = v.spec.MemoryMaxMiB
	}
	// microvm owns its direct-boot properties. The generic -kernel/-append
	// options are accepted by QEMU but ignored by this machine and fall
	// through to SeaBIOS, so keep them on -machine itself.
	machineArg := machineOptions()
	machineArg += ",kernel=" + qemuValue(dk.Vmlinux) + ",append=" + qemuValue(dk.Cmdline)
	if dk.Initrd != "" {
		machineArg += ",initrd=" + qemuValue(dk.Initrd)
	}
	if len(v.spec.Shares) > 0 {
		machineArg += ",memory-backend=mem"
	}
	a := []string{
		"-machine", machineArg,
		"-cpu", cpuModel(v.spec.NestedVirt),
		"-smp", strconv.FormatUint(uint64(v.spec.VCPU), 10),
		"-m", strconv.FormatUint(mem, 10),
		"-nodefaults", "-no-user-config",
		"-display", "none",
		"-serial", "stdio",
		"-monitor", "none",
		"-qmp", "unix:" + qemuValue(filepath.Join(v.stateDir, "qmp.sock")) + ",server=on,wait=off",
		"-sandbox", "on,elevateprivileges=deny,spawn=deny,obsolete=deny,resourcecontrol=deny",
	}
	if v.incoming {
		a = append(a, "-incoming", "defer", "-S")
	}
	a = append(a, "-device", "virtio-rng-device", "-device", "virtio-balloon-device,deflate-on-oom=on")
	a = append(a, "-only-migratable")
	a = append(a, audioArgs(v.audio)...)

	all := append([]machine.Disk{{Path: root.Path, ReadOnly: root.ReadOnly}}, v.spec.Disks...)
	for i, d := range all {
		id := fmt.Sprintf("disk%d", i)
		ro := "off"
		if d.ReadOnly {
			ro = "on"
		}
		device := "virtio-blk-device,drive=" + id
		a = append(a, "-drive", fmt.Sprintf("if=none,id=%s,file=%s,format=raw,readonly=%s", id, qemuValue(d.Path), ro), "-device", device)
	}
	for _, n := range v.spec.Net {
		switch nn := n.(type) {
		case machine.UserMode:
			if v.stack == nil {
				return nil, errors.New("qemu: usermode stack missing")
			}
			a = append(a, "-netdev", fmt.Sprintf("dgram,id=net0,local.type=unix,local.path=%s,remote.type=unix,remote.path=%s", qemuValue(v.stack.VMPath), qemuValue(v.stack.HostPath)), "-device", "virtio-net-device,netdev=net0,mac="+v.stack.GuestMAC)
		case machine.TAP:
			a = append(a, "-netdev", "tap,id=net0,ifname="+qemuValue(nn.Device)+",script=no,downscript=no", "-device", "virtio-net-device,netdev=net0,mac="+qemuValue(nn.MAC))
		}
	}
	if v.spec.VSockCID != 0 {
		a = append(a, "-device", fmt.Sprintf("vhost-vsock-device,guest-cid=%d", v.spec.VSockCID)+v.vhostArg())
	}
	if len(v.spec.Shares) > 0 {
		a = append(a, "-object", fmt.Sprintf("memory-backend-memfd,id=mem,size=%dM,share=on", mem))
		for i, sh := range v.spec.Shares {
			id := fmt.Sprintf("vfs%d", i)
			// PCI/MSI keeps shares off the board's eight MMIO transports.
			a = append(a, "-chardev", fmt.Sprintf("socket,id=%s,path=%s", id, qemuValue(filepath.Join(v.stateDir, fmt.Sprintf("virtiofs-%d.sock", i)))), "-device", fmt.Sprintf("vhost-user-fs-pci,chardev=%s,tag=%s", id, sh.Tag))
		}
	}
	return a, nil
}

// QEMU's keyval parser represents a literal comma by doubling it.
func qemuValue(s string) string { return strings.ReplaceAll(s, ",", ",,") }

func (v *vm) Stop(ctx context.Context, graceful bool) error {
	if err := v.opMu.LockContext(ctx); err != nil {
		return err
	}
	defer v.opMu.Unlock()
	v.mu.Lock()
	q, p, state := v.qmp, v.process, v.state
	v.mu.Unlock()
	if graceful && p != nil && !p.exited() && q != nil {
		if state == machine.StatePaused {
			_, _ = q.command(ctx, "cont", nil)
		}
		if v.shutdownGuest != nil {
			v.mu.Lock()
			v.state = machine.StateRunning
			v.mu.Unlock()
			_ = v.shutdownGuest(ctx)
		} else {
			_, _ = q.command(ctx, "system_powerdown", nil)
		}
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
		}
	}
	v.cleanup()
	v.mu.Lock()
	if v.state != machine.StateDestroyed {
		v.state = machine.StateStopped
	}
	v.mu.Unlock()
	return nil
}
func (v *vm) cleanup() {
	v.mu.Lock()
	p, q := v.process, v.qmp
	v.qmp = nil
	v.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.stop(ctx)
	if v.vhost != nil {
		_ = v.vhost.Close()
		v.vhost = nil
	}
	if q != nil {
		_ = q.Close()
	}
	for _, c := range v.virtiofs {
		c.stop(ctx)
	}
	v.virtiofs = nil
	if v.cancel != nil {
		v.cancel()
		v.cancel = nil
	}
	if v.stack != nil {
		_ = v.stack.Close()
		v.stack = nil
	}
	_ = os.Remove(filepath.Join(v.stateDir, "qemu.pid"))
	_ = os.Remove(filepath.Join(v.stateDir, "qmp.sock"))
}
func (v *vm) Destroy(ctx context.Context) error {
	if err := v.opMu.LockContext(ctx); err != nil {
		return err
	}
	defer v.opMu.Unlock()
	// Keep teardown and deletion under one lock; Start cannot interleave.
	v.cleanup()
	err := os.RemoveAll(v.stateDir)
	v.releaseReservations()
	v.mu.Lock()
	v.state = machine.StateDestroyed
	v.mu.Unlock()
	return err
}

// Close releases host reservations without deleting the persistent root disk.
func (v *vm) Close() error {
	v.opMu.Lock()
	defer v.opMu.Unlock()
	v.cleanup()
	v.releaseReservations()
	return nil
}

func (v *vm) releaseReservations() {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, l := range v.listeners {
		_ = l.Close()
	}
	v.listeners = nil
	if v.vhost != nil {
		_ = v.vhost.Close()
		v.vhost = nil
	}
	if v.lock != nil {
		_ = v.lock.Close()
		v.lock = nil
	}
	v.state = machine.StateDestroyed // handle is closed; persistent files remain
}
func (v *vm) State(ctx context.Context) (machine.State, error) {
	locked := v.opMu.TryLock()
	if locked {
		defer v.opMu.Unlock()
	}
	v.mu.Lock()
	state, p, err := v.state, v.process, v.failure
	v.mu.Unlock()
	if locked && (state == machine.StateRunning || state == machine.StatePaused) {
		if p != nil && p.exited() {
			v.mu.Lock()
			v.state = machine.StateStopped
			v.mu.Unlock()
			return machine.StateStopped, p.err
		}
		if err != nil {
			return machine.StateStopped, err
		}
		for _, helper := range v.virtiofs {
			if helper.exited() {
				return machine.StateStopped, fmt.Errorf("virtiofsd exited: %v", helper.err)
			}
		}
	}
	return state, nil
}
func (v *vm) VSock(ctx context.Context, port uint32) (net.Conn, error) {
	v.mu.Lock()
	state := v.state
	cid := v.spec.VSockCID
	token := v.authToken
	v.mu.Unlock()
	if cid == 0 {
		return nil, machine.ErrVSockUnsupported
	}
	if state != machine.StateRunning {
		return nil, fmt.Errorf("%w: VSock requires running VM", machine.ErrInvalidState)
	}
	c, err := dialVSock(ctx, cid, port)
	if err != nil {
		return nil, err
	}
	if token != "" {
		deadline := time.Now().Add(5 * time.Second)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		_ = c.SetWriteDeadline(deadline)
		if _, err = io.WriteString(c, token); err != nil {
			c.Close()
			return nil, err
		}
		_ = c.SetWriteDeadline(time.Time{})
	}
	return c, nil
}

// SetVSockAuthToken enables the clawk guest services' authentication prefix.
// Generic machine-library guests keep their own protocol unless opted in.
func (v *vm) SetVSockAuthToken(token string) error {
	v.opMu.Lock()
	defer v.opMu.Unlock()
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state == machine.StateRunning || v.state == machine.StatePaused || v.state == machine.StateDestroyed {
		return machine.ErrInvalidState
	}
	if len(token) != 64 {
		return errors.New("QEMU vsock token must have 64 characters")
	}
	v.authToken = token
	return nil
}

func (v *vm) Pause(ctx context.Context) error {
	if err := v.opMu.LockContext(ctx); err != nil {
		return err
	}
	defer v.opMu.Unlock()
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state == machine.StatePaused {
		return nil
	}
	if v.state != machine.StateRunning || v.qmp == nil {
		return fmt.Errorf("%w: pause in state %s", machine.ErrInvalidState, v.state)
	}
	if _, err := v.qmp.command(ctx, "stop", nil); err != nil {
		return err
	}
	if s, err := v.qmp.status(ctx); err != nil {
		return fmt.Errorf("qemu: checking pause: %w", err)
	} else if s != "paused" {
		return fmt.Errorf("qemu: expected paused state, got %q", s)
	}
	v.state = machine.StatePaused
	return nil
}

func (v *vm) Resume(ctx context.Context) error {
	if err := v.opMu.LockContext(ctx); err != nil {
		return err
	}
	defer v.opMu.Unlock()
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state == machine.StateRunning {
		return nil
	}
	if v.state != machine.StatePaused || v.qmp == nil {
		return fmt.Errorf("%w: resume in state %s", machine.ErrInvalidState, v.state)
	}
	if _, err := v.qmp.command(ctx, "cont", nil); err != nil {
		return err
	}
	if s, err := v.qmp.status(ctx); err != nil {
		return fmt.Errorf("qemu: checking resume: %w", err)
	} else if s != "running" {
		return fmt.Errorf("qemu: expected running state, got %q", s)
	}
	v.state = machine.StateRunning
	return nil
}

var _ machine.Machine = (*vm)(nil)
var _ machine.Pauseable = (*vm)(nil)

func (v *vm) SetGuestLifecycle(syncGuest, shutdownGuest func(context.Context) error) {
	v.opMu.Lock()
	defer v.opMu.Unlock()
	v.syncGuest = syncGuest
	v.shutdownGuest = shutdownGuest
}

func (v *vm) SetPrepareBoot(f func() error) {
	v.opMu.Lock()
	defer v.opMu.Unlock()
	v.prepareBoot = f
}
