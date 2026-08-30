//go:build linux && amd64

package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/clawkwork/clawk/internal/guestbuild"
	"github.com/clawkwork/clawk/internal/guestcfg"
	"github.com/clawkwork/clawk/internal/vsockproto"
	"github.com/clawkwork/clawk/machine"
	"github.com/clawkwork/clawk/machine/guest"
	"github.com/clawkwork/clawk/machine/kernel"
	"github.com/clawkwork/clawk/machine/qemu"
	"github.com/stretchr/testify/require"
)

func TestQEMUGuest(t *testing.T) {
	if os.Getenv("TEST_QEMU_BOOT") == "" {
		t.Skip("set TEST_QEMU_BOOT=1 on a host with KVM and virtiofsd")
	}
	t.Setenv("CLAWK_AUDIO_INPUT", "0")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	require.NoError(t, qemu.Preflight(ctx))
	cache := os.Getenv("TEST_GUEST_BOOT_CACHE")
	if cache == "" {
		cache = t.TempDir()
	}
	bins, err := guestbuild.Build(ctx, cache, qemu.GuestArch())
	require.NoError(t, err)
	k, err := kernel.FetchQEMU(ctx, cache, qemu.GuestArch(), os.Getenv("TEST_QEMU_KERNEL"))
	require.NoError(t, err)
	dir := t.TempDir()
	share := t.TempDir()
	ro := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(share, "host.txt"), []byte("host truth"), 0o600))
	cfg := filepath.Join(dir, "guestcfg.img")
	spec := machine.Spec{ID: dir, VCPU: 1, MemoryMiB: 512, MemoryMaxMiB: 768, Boot: machine.DirectKernel{Vmlinux: k, Cmdline: "console=" + qemu.Console() + " root=/dev/vda rw psi=1 init=" + guestcfg.InitPath + " clawk.cfg=/dev/vdb"}, RootFS: machine.OCIImage{Ref: testImage, CacheDir: filepath.Join(cache, "oci"), Platform: "linux/" + qemu.GuestArch(), Inject: []machine.InjectFile{{GuestPath: guestcfg.InitPath, HostPath: bins.Init, Mode: 0o755}, {GuestPath: guestcfg.AgentPath, HostPath: bins.Agent, Mode: 0o755}, {GuestPath: guestcfg.TimeSyncPath, HostPath: bins.TimeSync, Mode: 0o755}}}, Disks: []machine.Disk{{Path: cfg, ReadOnly: true}}, Shares: []machine.Share{{Tag: "workspace", HostPath: share}, {Tag: "readonly", HostPath: ro, ReadOnly: true}}}
	// Exercise the PCI/MSI share devices with a realistic number of exports.
	for i := 2; i < 16; i++ {
		spec.Shares = append(spec.Shares, machine.Share{Tag: fmt.Sprintf("export%d", i), HostPath: t.TempDir()})
	}
	backend, err := machine.Get("qemu")
	require.NoError(t, err)
	m, err := backend.New(ctx, spec, dir)
	require.NoError(t, err)
	t.Cleanup(func() {
		stop, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_ = m.Stop(stop, false)
		_ = m.(interface{ Close() error }).Close()
		if t.Failed() {
			if b, e := os.ReadFile(filepath.Join(dir, "console.log")); e == nil {
				t.Logf("console:\n%s", b)
			}
			for i := range spec.Shares {
				if b, e := os.ReadFile(filepath.Join(dir, fmt.Sprintf("virtiofs-%d.log", i))); e == nil {
					t.Logf("virtiofs:\n%s", b)
				}
			}
		}
	})
	offset := m.(interface{ HostVSockPort(uint32) uint32 }).HostVSockPort(0)
	manifest := guestcfg.Manifest{User: &guestcfg.User{Name: "agent", UID: os.Getuid(), GID: os.Getgid()}, Mounts: []guestcfg.Mount{{Tag: "workspace", Path: "/workspace"}, {Tag: "readonly", Path: "/readonly", ReadOnly: true}}, Services: []guestcfg.Service{{Name: "pty-agent", Path: guestcfg.AgentPath, Env: []string{fmt.Sprintf("CLAWK_HOST_PORT_OFFSET=%d", offset)}}}}
	for i := 2; i < 16; i++ {
		manifest.Mounts = append(manifest.Mounts, guestcfg.Mount{Tag: fmt.Sprintf("export%d", i), Path: fmt.Sprintf("/export%d", i)})
	}
	require.NoError(t, guestcfg.WriteDisk(manifest, cfg))
	require.NoError(t, m.Create(ctx))
	require.NoError(t, m.Start(ctx))
	run := func(script string) string {
		c := dialAgent(t, ctx, m)
		defer c.Close()
		out, exit := runOverAgent(t, c, vsockproto.Handshake{Cmd: "/bin/sh", Args: []string{"-c", script}, User: "agent"}, "")
		require.EqualValues(t, 0, exit, out)
		return out
	}
	require.Contains(t, run("set -e; for d in /export*; do echo shared > \"$d/proof\"; done; id; cat /workspace/host.txt; echo guest > /workspace/guest.txt; if touch /readonly/forbidden 2>/dev/null; then exit 23; fi; echo retained > /home/agent/disk-proof; echo memory > /tmp/memory-proof; sync"), "host truth")
	content, err := os.ReadFile(filepath.Join(share, "guest.txt"))
	require.NoError(t, err)
	require.Contains(t, string(content), "guest")
	info, err := os.Stat(filepath.Join(share, "guest.txt"))
	require.NoError(t, err)
	owner := info.Sys().(*syscall.Stat_t)
	require.EqualValues(t, os.Getuid(), owner.Uid)
	require.EqualValues(t, os.Getgid(), owner.Gid)

	shell := dialAgent(t, ctx, m)
	out, code := runOverAgent(t, shell, vsockproto.Handshake{User: "agent", Rows: 24, Cols: 80}, "echo shell-ok; exit\n")
	shell.Close()
	require.Zero(t, code, out)
	require.Contains(t, strings.ReplaceAll(out, "\r\n", "\n"), "\nshell-ok\n")

	balloon := m.(interface {
		SetMemoryTarget(context.Context, uint64) error
		MemoryTarget(context.Context) (uint64, error)
	})
	before, err := guest.FetchStats(ctx, m)
	require.NoError(t, err)
	require.NoError(t, balloon.SetMemoryTarget(ctx, 512<<20))
	require.Eventually(t, func() bool {
		current, err := balloon.MemoryTarget(ctx)
		return err == nil && current <= 544<<20
	}, 20*time.Second, 100*time.Millisecond)
	after, err := guest.FetchStats(ctx, m)
	require.NoError(t, err)
	// With deflate-on-OOM, Linux keeps MemTotal fixed and charges balloon
	// pages as used memory. Check the available memory and QMP's actual size.
	require.Less(t, after.AvailableKiB, before.AvailableKiB-128*1024)
	require.NoError(t, balloon.SetMemoryTarget(ctx, 768<<20))
	require.Eventually(t, func() bool {
		current, err := balloon.MemoryTarget(ctx)
		return err == nil && current >= 740<<20
	}, 20*time.Second, 100*time.Millisecond)

	require.NoError(t, m.(machine.Pauseable).Pause(ctx))
	state, err := m.State(ctx)
	require.NoError(t, err)
	require.Equal(t, machine.StatePaused, state)
	require.NoError(t, m.(machine.Pauseable).Resume(ctx))
	require.Contains(t, run("cat /home/agent/disk-proof"), "retained")
	// Guest-initiated ports use a private host range and kernel peer identity.
	listener, err := m.(machine.VSockListener).VSockListen(ctx, 1028)
	require.NoError(t, err)
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, e := listener.Accept()
		if e == nil {
			accepted <- c
		}
	}()

	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(10 * time.Second):
		t.Fatal("guest did not reach its mapped reverse-forward port")
	}
	require.NoError(t, m.(interface {
		SetMemoryTarget(context.Context, uint64) error
	}).SetMemoryTarget(ctx, 512<<20))
	// No stateful session remains open during the suspend operation.
	run("sync")
	suspend := filepath.Join(dir, "suspend")
	require.NoError(t, m.(machine.Snapshottable).Snapshot(ctx, suspend))
	require.Contains(t, run("cat /tmp/memory-proof"), "memory")
	require.NoError(t, m.(machine.Suspendable).Suspend(ctx, suspend))
	require.NoError(t, m.(machine.Snapshottable).Restore(ctx, suspend))
	require.NoError(t, waitQEMUGuest(ctx, m))
	require.Contains(t, run("set -e; cat /tmp/memory-proof; cat /workspace/host.txt; cat /home/agent/disk-proof"), "retained")
	require.NoError(t, m.(machine.Suspendable).Suspend(ctx, suspend))
	require.NoError(t, os.WriteFile(filepath.Join(share, "host.txt"), []byte("edited while suspended"), 0o600))
	err = m.(machine.Snapshottable).Restore(ctx, suspend)
	require.ErrorContains(t, err, "host files")
	require.NoError(t, m.Start(ctx))
	require.True(t, strings.Contains(run("set -e; test ! -e /tmp/memory-proof; cat /workspace/host.txt; cat /home/agent/disk-proof"), "edited while suspended"))
	if os.Getenv("TEST_QEMU_FAULTS") == "1" {
		runQEMUFaultChecks(t, ctx, m, dir, share, run)
		return // the final fault check destroys the machine
	}
	require.NoError(t, m.Stop(ctx, false))
	state, err = m.State(ctx)
	require.NoError(t, err)
	require.Equal(t, machine.StateStopped, state)
}

func waitQEMUGuest(ctx context.Context, m machine.Machine) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		attempt, done := context.WithTimeout(ctx, 2*time.Second)
		conn, err := m.VSock(attempt, agentPort)
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			hs, _ := vsockproto.MarshalHandshake(vsockproto.Handshake{Cmd: "/bin/sh", Args: []string{"-c", "exit 0"}, Rows: 24, Cols: 80})
			err = vsockproto.WriteFrame(conn, vsockproto.FrameHandshake, hs)
			if err == nil {
				for {
					typ, _, e := vsockproto.ReadFrame(conn)
					err = e
					if err != nil || typ == vsockproto.FrameExit {
						break
					}
				}
			}
			conn.Close()
		}
		done()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("guest did not recover: %w", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}
