//go:build linux && amd64

package qemu

import (
	"bufio"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clawkwork/clawk/machine"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestQMPCancellationWithoutDeadline(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	q := &qmpClient{c: a, r: bufio.NewReader(a)}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _, _ = bufio.NewReader(b).ReadBytes('\n'); cancel() }()
	start := time.Now()
	_, err := q.command(ctx, "query-status", nil)
	require.Error(t, err)
	require.Less(t, time.Since(start), time.Second)
}

func TestQueuedLifecycleOperationsHonorCancellation(t *testing.T) {
	v := &vm{state: machine.StateRunning}
	v.opMu.Lock() // a snapshot or another lifecycle command is in progress
	defer v.opMu.Unlock()
	ops := map[string]func(context.Context) error{
		"create": v.Create, "start": v.Start, "pause": v.Pause, "resume": v.Resume,
		"stop":     func(ctx context.Context) error { return v.Stop(ctx, false) },
		"destroy":  v.Destroy,
		"snapshot": func(ctx context.Context) error { return v.Snapshot(ctx, "unused") },
		"restore":  func(ctx context.Context) error { return v.Restore(ctx, "unused") },
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- op(ctx) }()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.DeadlineExceeded)
			case <-time.After(time.Second):
				t.Fatal("queued operation ignored its deadline")
			}
		})
	}
	require.Equal(t, machine.StateRunning, v.state, "cancelled operations must not mutate state")
}
func TestChildExitCanBeObservedRepeatedly(t *testing.T) {
	p, err := launch(exec.Command("/bin/sh", "-c", "exit 7"), filepath.Join(t.TempDir(), "child.log"))
	require.NoError(t, err)
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		t.Fatal("child not reaped")
	}
	require.Error(t, p.err)
	require.True(t, p.exited())
	require.True(t, p.exited())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	p.stop(ctx)
	p.stop(ctx)
}
func TestChildStopReapsProcess(t *testing.T) {
	p, err := launch(exec.Command("/bin/sleep", "60"), filepath.Join(t.TempDir(), "child.log"))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p.stop(ctx)
	require.True(t, p.exited())
	require.ErrorIs(t, unix.Kill(p.cmd.Process.Pid, 0), unix.ESRCH)
}
func TestShareDigestDetectsContentAndIdentityChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(path, []byte("before"), 0o600))
	shares := []machine.Share{{Tag: "work", HostPath: dir}}
	before, err := treeDigest(context.Background(), shares)
	require.NoError(t, err)
	st, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("after!"), 0o600))
	require.NoError(t, os.Chtimes(path, st.ModTime(), st.ModTime()))
	after, err := treeDigest(context.Background(), shares)
	require.NoError(t, err)
	require.NotEqual(t, before, after)
	require.NoError(t, os.Rename(path, path+"-old"))
	require.NoError(t, os.WriteFile(path, []byte("after!"), 0o600))
	replaced, err := treeDigest(context.Background(), shares)
	require.NoError(t, err)
	require.NotEqual(t, after, replaced)
}
func TestShareDigestDoesNotFollowSymlinkAndRejectsSpecialFile(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.WriteFile(outside, []byte("one"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "link")))
	shares := []machine.Share{{Tag: "work", HostPath: dir}}
	a, err := treeDigest(context.Background(), shares)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(outside, []byte("two"), 0o600))
	b, err := treeDigest(context.Background(), shares)
	require.NoError(t, err)
	require.Equal(t, a, b)
	require.NoError(t, unix.Mkfifo(filepath.Join(dir, "pipe"), 0o600))
	_, err = treeDigest(context.Background(), shares)
	require.ErrorContains(t, err, "special file")
}
func TestVersions(t *testing.T) {
	require.True(t, versionAtLeast("QEMU emulator version 10.2.2", 10, 2))
	require.False(t, versionAtLeast("QEMU emulator version 9.2.0", 10, 2))
	require.True(t, versionAtLeast("virtiofsd 1.14.0", 1, 14))
	require.False(t, versionAtLeast("unknown", 1, 14))
}

func TestQEMUVersionRequirements(t *testing.T) {
	for _, tc := range []struct {
		version string
		ok      bool
	}{
		{"QEMU emulator version 10.1.0 (qemu-10.1.0-7.fc43)", true},
		{"QEMU emulator version 10.1.5 (qemu-10.1.5-1.fc43)", true},
		{"QEMU emulator version 10.2.2", true},
		{"QEMU emulator version 11.0.0", true},
		{"QEMU emulator version 10.0.7", false},
		{"QEMU emulator version 9.2.4", false},
		{"unknown", false},
	} {
		t.Run(tc.version, func(t *testing.T) {
			err := checkQEMUVersion(tc.version)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, qemuVersionRequirement)
				require.ErrorContains(t, err, tc.version)
			}
		})
	}
}

func TestShareDigestFollowsExportRoot(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "export")
	require.NoError(t, os.Symlink(dir, link))
	path := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(path, []byte("before"), 0o600))
	shares := []machine.Share{{Tag: "work", HostPath: link}}
	a, err := treeDigest(context.Background(), shares)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("after"), 0o600))
	b, err := treeDigest(context.Background(), shares)
	require.NoError(t, err)
	require.NotEqual(t, a, b)
}

func TestCIDReservationAndExclusiveMachineOwnership(t *testing.T) {
	if os.Getenv("TEST_QEMU_BOOT") == "" {
		t.Skip("requires KVM and vhost-vsock")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	spec := machine.Spec{ID: "cid-collision", VCPU: 1, MemoryMiB: 128, Boot: machine.DirectKernel{Vmlinux: "kernel"}, RootFS: machine.RawDisk{Path: "disk"}}
	dir := t.TempDir()
	a, err := (backend{}).New(ctx, spec, dir)
	require.NoError(t, err)
	defer a.(*vm).Close()
	_, err = (backend{}).New(ctx, spec, dir)
	require.ErrorContains(t, err, "owned")
	// Same ID derives the same candidate CID in a different state directory.
	b, err := (backend{}).New(ctx, spec, t.TempDir())
	require.NoError(t, err)
	defer b.(*vm).Close()
	require.NotEqual(t, a.(*vm).spec.VSockCID, b.(*vm).spec.VSockCID)
	require.NotEqual(t, a.(*vm).HostVSockPort(1028), b.(*vm).HostVSockPort(1028))
}

// This exercises the real board, HDA controller/codec, and migration stream.
// The null host backend makes it runnable on headless KVM runners; microphone
// capture with a guest driver and desktop audio server is a separate check.
func TestHDADeviceMigration(t *testing.T) {
	if os.Getenv("TEST_QEMU_BOOT") == "" {
		t.Skip("requires KVM and QEMU")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	board := machineOptions()
	dir := t.TempDir()
	launchVM := func(incoming bool) *vm {
		t.Helper()
		sock := filepath.Join(dir, "qmp.sock")
		_ = os.Remove(sock)
		args := []string{"-machine", board, "-cpu", cpuModel(false), "-m", "64", "-S", "-nodefaults", "-display", "none", "-only-migratable", "-qmp", "unix:" + sock + ",server=on,wait=off", "-sandbox", "on,elevateprivileges=deny,spawn=deny,obsolete=deny,resourcecontrol=deny"}
		args = append(args, audioArgs("none")...)
		if incoming {
			args = append(args, "-incoming", "defer")
		}
		p, err := launch(exec.Command(BinaryName(), args...), filepath.Join(dir, "qemu.log"))
		require.NoError(t, err)
		t.Cleanup(func() {
			cleanup, done := context.WithTimeout(context.Background(), 3*time.Second)
			defer done()
			p.stop(cleanup)
			if t.Failed() {
				b, _ := os.ReadFile(filepath.Join(dir, "qemu.log"))
				t.Log(string(b))
			}
		})
		require.NoError(t, waitPath(ctx, sock, p))
		q, err := dialQMP(ctx, sock)
		require.NoError(t, err)
		t.Cleanup(func() { _ = q.Close() })
		return &vm{stateDir: dir, qmp: q, process: p}
	}
	source := launchVM(false)
	state := filepath.Join(dir, "audio.state")
	require.NoError(t, source.transfer(ctx, state, false))
	source.process.stop(ctx)
	destination := launchVM(true)
	require.NoError(t, destination.transfer(ctx, state, true))
	pci, err := destination.qmp.command(ctx, "query-pci", nil)
	require.NoError(t, err)
	require.True(t, strings.Contains(string(pci), "hda0"), string(pci))
	_, err = destination.qmp.command(ctx, "cont", nil)
	require.NoError(t, err)
}
