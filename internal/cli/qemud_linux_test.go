//go:build linux && amd64

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/clawkwork/clawk/internal/serialport/serialporttest"
	"github.com/clawkwork/clawk/internal/vsockclient"
	"github.com/clawkwork/clawk/internal/vsockproto"
	"github.com/clawkwork/clawk/internal/vzdctl"
	"github.com/clawkwork/clawk/machine"
	"github.com/clawkwork/clawk/machine/qemu"
	"github.com/mdlayher/vsock"
	"github.com/stretchr/testify/require"
)

// Alpine has no sudo. Boot the actual default image through the public
// provider to cover PID 1's environment, sudo policy and cold-boot updates.
func TestQEMUDefaultImage(t *testing.T) {
	if os.Getenv("TEST_QEMU_BOOT") == "" {
		t.Skip("set TEST_QEMU_BOOT=1 on a rootless KVM host")
	}
	dir, err := os.MkdirTemp("", "clawk-image-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	s := config.NewStoreAt(dir)
	previousStore := store
	store = s
	t.Cleanup(func() { store = previousStore })
	if cache := os.Getenv("TEST_GUEST_BOOT_CACHE"); cache != "" {
		require.NoError(t, os.Symlink(cache, s.CacheDir()))
	}
	t.Setenv("CLAWK_TEST_QEMUD_STORE", dir)
	t.Setenv("CLAWK_AUDIO_INPUT", "0")
	repo := filepath.Join(dir, "repo")
	require.NoError(t, os.Mkdir(repo, 0700))
	sb := &config.Sandbox{Name: "box", Provider: config.ProviderQEMU, CPU: 2, MemoryMiB: 1024, DiskMiB: 8192, Image: defaultImage, Phases: []config.Phase{{Repo: repo, Worktree: repo, InPlace: true, OnCreate: []string{"echo create >> hooks.log"}, Setup: []string{"echo up >> hooks.log"}}}}
	require.NoError(t, s.Save(sb))
	p := sandbox.NewQEMUProvider(s)
	t.Cleanup(func() {
		if t.Failed() {
			for _, name := range []string{"qemud.log", "console.log"} {
				t.Log(sandbox.LogTail(filepath.Join(s.VMDir(sb.Key()), name), 30, name))
			}
		}
		_ = p.Destroy(sb)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	require.NoError(t, bootSandbox(p, sb))
	hooks, err := os.ReadFile(filepath.Join(repo, "hooks.log"))
	require.NoError(t, err)
	require.Equal(t, "create\nup\n", string(hooks))
	sock := filepath.Join(s.VMDir(sb.Key()), "agent.sock")
	run := func(script string) {
		t.Helper()
		out, code, err := vsockclient.Output(ctx, sock, 0, sandbox.GuestUser, "/bin/sh", "-c", "set -eu; "+script)
		require.NoError(t, err, out)
		require.Zero(t, code, out)
	}
	identity := fmt.Sprintf("test \"$(id -un)\" = agent; test \"$(id -u)\" = %d; test \"$(id -g)\" = %d; test \"$(sudo -n id -u)\" = 0; sudo -n visudo -c", os.Getuid(), os.Getgid())
	run(identity + "; claude --version; codex --version; echo guest-only > /home/agent/boot-marker; echo persistent > /home/agent/.claude/boot-marker")
	require.NoError(t, p.Stop(sb))
	console, err := os.ReadFile(filepath.Join(s.VMDir(sb.Key()), "console.log"))
	require.NoError(t, err)
	require.Contains(t, string(console), "Power down", "SIGTERM must reach guest init before host services close")
	// Start directly as the idle-restart path does: no preceding Create call.
	require.NoError(t, p.StartContext(ctx, sb))
	run(identity + "; test ! -e /home/agent/boot-marker; test \"$(cat /home/agent/.claude/boot-marker)\" = persistent; echo saved > /home/agent/boot-marker")
	ctl := vzdctl.NewClient(vzdctl.SocketPath(s.VMDir(sb.Key())))
	require.NoError(t, ctl.Suspend(ctx))
	require.Eventually(t, func() bool { status, _ := p.Status(sb); return status == "stopped" }, 20*time.Second, 50*time.Millisecond)
	// Simulate a source build update with the same host version string.
	suspendDir := suspendStateDir(s.VMDir(sb.Key()))
	meta, ok := machine.ReadSuspendMeta(suspendDir)
	require.True(t, ok)
	meta.GuestSources = "obsolete-init"
	require.NoError(t, machine.WriteSuspendMeta(suspendDir, meta))
	require.NoError(t, p.StartContext(ctx, sb))
	state, err := ctl.Lifecycle(ctx)
	require.NoError(t, err)
	require.False(t, state.Restored)
	run(identity + "; test ! -e /home/agent/boot-marker; test \"$(cat /home/agent/.claude/boot-marker)\" = persistent")
}

// Exercise Provider.Start's real self-exec path without a production test flag.
func init() {
	testProviderDaemon = func() {
		if len(os.Args) != 3 || os.Args[1] != "__qemud" {
			return
		}
		store = config.NewStoreAt(os.Getenv("CLAWK_TEST_QEMUD_STORE"))
		if err := runQemud(nil, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
}

// Re-exec the actual daemon against an isolated Store, without exposing a
// test-only state-directory override in the shipped CLI.
func TestQEMUDProcess(t *testing.T) {
	dir := os.Getenv("CLAWK_TEST_QEMUD_STORE")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	store = config.NewStoreAt(dir)
	require.NoError(t, runQemud(nil, []string{"default/box"}))
}

func TestQEMUDevelopmentWorkflow(t *testing.T) {
	if os.Getenv("TEST_QEMU_BOOT") == "" {
		t.Skip("set TEST_QEMU_BOOT=1 on a rootless KVM host")
	}
	audioPCM := os.Getenv("TEST_QEMU_AUDIO_PCM") == "1"
	if audioPCM {
		setupQEMUTestAudio(t)
	}
	audioDevice := os.Getenv("TEST_QEMU_AUDIO_DEVICE") == "1" || audioPCM
	if audioDevice {
		t.Setenv("CLAWK_AUDIO_INPUT", "1")
	} else {
		t.Setenv("CLAWK_AUDIO_INPUT", "0")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	// Short path leaves room for the Linux sockaddr_un limit.
	dir, err := os.MkdirTemp("", "clawk-qemud-")
	require.NoError(t, err)
	defer os.RemoveAll(dir)
	s := config.NewStoreAt(dir)
	if cache := os.Getenv("TEST_GUEST_BOOT_CACHE"); cache != "" {
		require.NoError(t, os.Symlink(cache, s.CacheDir()))
	}
	repo := filepath.Join(dir, "repo")
	require.NoError(t, os.Mkdir(repo, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "host.txt"), []byte("host files are live"), 0o600))
	// Compile the dependency-free probe for the amd64 guest.
	build := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", filepath.Join(repo, "probe"), "./testdata/qemu-probe")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+qemu.GuestArch())
	b, err := build.CombinedOutput()
	require.NoError(t, err, string(b))
	sshSock := filepath.Join(dir, "ssh.sock")
	ssh, err := net.Listen("unix", sshSock)
	require.NoError(t, err)
	defer ssh.Close()
	go func() {
		for {
			c, err := ssh.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				request := make([]byte, 5)
				if _, err := io.ReadFull(c, request); err == nil && request[4] == 11 {
					_, _ = c.Write([]byte{0, 0, 0, 5, 12, 0, 0, 0, 0})
				}
			}()
		}
	}()
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "reverse forward reached") }))
	defer host.Close()
	_, port, err := net.SplitHostPort(host.Listener.Addr().String())
	require.NoError(t, err)
	hostPort, err := strconv.Atoi(port)
	require.NoError(t, err)
	board := echoQEMUSerialBoard(t)
	serialPath := filepath.Join(dir, "serial")
	require.NoError(t, os.Symlink(board.path, serialPath))
	sb := &config.Sandbox{Name: "box", Provider: config.ProviderQEMU, CPU: 1, MemoryMiB: 512, MemoryMaxMiB: 768, DiskMiB: 1024, Image: "docker.io/library/alpine:3.20", Kernel: os.Getenv("TEST_QEMU_KERNEL"), Phases: []config.Phase{{Repo: repo, Worktree: repo, InPlace: true}}, ReverseForwards: []config.PortForward{{HostPort: hostPort, GuestPort: 15432}}}
	sb.NestedVirt = os.Getenv("TEST_QEMU_NESTED") == "1"
	sb.Serials = []config.SerialDevice{{HostPath: serialPath, GuestName: "ttyCLAWK0"}}
	require.NoError(t, s.Save(sb))
	p := sandbox.NewQEMUProvider(s)
	require.NoError(t, p.Create(sb))
	self, err := os.Executable()
	require.NoError(t, err)
	start := func() (*exec.Cmd, <-chan struct{}) {
		cmd := exec.Command(self, "-test.run=^TestQEMUDProcess$", "-test.v")
		cmd.Env = append(os.Environ(), "CLAWK_TEST_QEMUD_STORE="+dir, "CLAWK_SSH_AUTH_SOCK="+sshSock)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		require.NoError(t, cmd.Start())
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		return cmd, done
	}
	cmd, done := start()
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("daemon failed to shut down cleanly")
		}
		if t.Failed() {
			for _, name := range []string{"qemud.log", "console.log"} {
				b, _ := os.ReadFile(filepath.Join(s.VMDir(sb.Key()), name))
				t.Logf("%s:\n%s", name, b)
			}
		}
	}()
	sock := filepath.Join(s.VMDir(sb.Key()), "agent.sock")
	require.NoError(t, waitGuestAgent(ctx, sock))
	if audioDevice {
		args, err := os.ReadFile(filepath.Join(s.VMDir(sb.Key()), "qemu", "qemu.cmdline"))
		require.NoError(t, err)
		require.Contains(t, string(args), "hda-micro", "TEST_QEMU_AUDIO_DEVICE requires a supported desktop audio backend")
	}
	// A host process bypassing the private Unix proxy must not obtain a
	// guest root session by talking directly to Linux's AF_VSOCK socket.
	cidBytes, err := os.ReadFile(filepath.Join(s.VMDir(sb.Key()), "qemu", "vsock.cid"))
	require.NoError(t, err)
	var cid uint32
	_, err = fmt.Sscan(string(cidBytes), &cid)
	require.NoError(t, err)
	raw, err := vsock.Dial(cid, vsockproto.AgentVSockPort, nil)
	require.NoError(t, err)
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	hs, err := vsockproto.MarshalHandshake(vsockproto.Handshake{Cmd: "/bin/sh", Args: []string{"-c", "touch /tmp/no-auth"}, User: "root"})
	require.NoError(t, err)
	require.NoError(t, vsockproto.WriteFrame(raw, vsockproto.FrameHandshake, hs))
	_, _, err = vsockproto.ReadFrame(raw)
	require.Error(t, err, "unauthenticated native vsock must be closed")
	raw.Close()
	guest := func(script string) string {
		c, stop := context.WithTimeout(ctx, 20*time.Second)
		defer stop()
		out, code, err := vsockclient.Output(c, sock, 0, sandbox.GuestUser, "/bin/sh", "-c", "set -e; "+script)
		require.NoError(t, err, out)
		require.Zero(t, code, out)
		return out
	}
	work := sandbox.WorkspaceRoot + "/repo"
	probe := work + "/probe"
	require.Contains(t, guest("test ! -e /tmp/no-auth; cat "+work+"/host.txt; echo guest > "+work+"/guest.txt; "+probe+"; wget -qO- http://127.0.0.1:15432"), "reverse forward reached")
	b, err = os.ReadFile(filepath.Join(repo, "guest.txt"))
	require.NoError(t, err)
	require.Contains(t, string(b), "guest")
	checkFeatures := func() {
		t.Helper()
		mode := "no-nested"
		if sb.NestedVirt {
			mode = "nested"
		}
		t.Log(guest(probe + " " + mode))
		if audioPCM {
			t.Log(guest(probe + " audio"))
		}
	}
	checkFeatures()
	checkSerial := func(speed int) {
		t.Helper()
		script := fmt.Sprintf("for i in $(seq 1 50); do test -e /dev/ttyCLAWK0 && break; sleep 0.1; done; exec 3<>/dev/ttyCLAWK0; stty -F /dev/ttyCLAWK0 raw -echo %d; printf serial-proof >&3; dd bs=1 count=12 <&3 2>/dev/null", speed)
		require.Contains(t, guest(script), "serial-proof")
		require.Equal(t, speed, serialporttest.Speed(t, int(board.master.Fd())))
	}
	checkSerial(115200)
	checkSerial(57600)
	ctl := vzdctl.NewClient(vzdctl.SocketPath(s.VMDir(sb.Key())))
	require.NoError(t, ctl.Pause(ctx))
	require.NoError(t, ctl.Resume(ctx))
	require.Contains(t, guest("cat "+work+"/host.txt"), "host files are live")
	// A full sync touches every export, catching IRQ/device-capacity mistakes
	// that a successful PTY handshake alone would miss.
	if sb.NestedVirt {
		guest(probe + " nested-start")
		guest("for i in $(seq 1 100); do test -f /tmp/nested-ready && exit 0; sleep 0.1; done; cat /tmp/nested.log; exit 1")
	}
	guest("echo retained > /tmp/daemon-memory-proof; sync")
	require.NoError(t, ctl.Suspend(ctx))
	select {
	case <-done:
		require.True(t, cmd.ProcessState.Success(), "suspend must exit cleanly")
	case <-ctx.Done():
		t.Fatal("daemon did not exit after suspend")
	}
	cmd, done = start()
	require.NoError(t, waitGuestAgent(ctx, sock))
	require.Contains(t, guest("cat /tmp/daemon-memory-proof; "+probe+"; wget -qO- http://127.0.0.1:15432"), "retained")
	if sb.NestedVirt {
		require.Contains(t, guest("touch /tmp/nested-continue; for i in $(seq 1 100); do test -f /tmp/nested-resumed && break; sleep 0.1; done; cat /tmp/nested.log /tmp/nested-resumed"), "inner guest resumed through KVM_RUN")
	}
	checkFeatures()
	checkSerial(38400)
	// Replace the device behind a stable host path, like a USB UART unplug
	// and reconnect. The next attachment must resolve and open the new tty.
	require.NoError(t, board.master.Close())
	board = echoQEMUSerialBoard(t)
	require.NoError(t, os.Remove(serialPath))
	require.NoError(t, os.Symlink(board.path, serialPath))
	checkSerial(19200)
	if os.Getenv("TEST_QEMU_FAULTS") == "1" {
		guest("echo disposable > /home/agent/daemon-crash-proof; echo persistent > /home/agent/.claude/daemon-crash-proof; sync")
		require.NoError(t, cmd.Process.Kill())
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("killed daemon did not exit")
		}
		cmd, done = start()
		require.NoError(t, waitGuestAgent(ctx, sock))
		require.Contains(t, guest("test ! -e /home/agent/daemon-crash-proof; cat /home/agent/.claude/daemon-crash-proof; "+probe+"; wget -qO- http://127.0.0.1:15432"), "persistent")
		checkFeatures()
		checkSerial(9600)
	}
}

func echoQEMUSerialBoard(t *testing.T) fakeBoard {
	t.Helper()
	board := newFakeBoard(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		buf := make([]byte, 4096)
		for ctx.Err() == nil {
			_ = board.master.SetDeadline(time.Now().Add(200 * time.Millisecond))
			n, err := board.master.Read(buf)
			if n > 0 {
				_, _ = board.master.Write(buf[:n])
			}
			if errors.Is(err, os.ErrClosed) {
				return
			}
			if err != nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
	}()
	return board
}

// Exercise the public provider lifecycle with concurrent VMs, live policy
// reload, forwarding, pause/restore, failed-start cleanup and idle shutdown.
func TestQEMUProviderLifecycle(t *testing.T) {
	if os.Getenv("TEST_QEMU_BOOT") == "" {
		t.Skip("set TEST_QEMU_BOOT=1 on a rootless KVM host")
	}
	dir, err := os.MkdirTemp("", "clawk-provider-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	s := config.NewStoreAt(dir)
	if cache := os.Getenv("TEST_GUEST_BOOT_CACHE"); cache != "" {
		require.NoError(t, os.Symlink(cache, s.CacheDir()))
	}
	t.Setenv("CLAWK_TEST_QEMUD_STORE", dir)
	t.Setenv("CLAWK_AUDIO_INPUT", "0")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	p := sandbox.NewQEMUProvider(s)
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "host-egress") }))
	defer host.Close()
	_, hostPort, err := net.SplitHostPort(host.Listener.Addr().String())
	require.NoError(t, err)
	probe := filepath.Join(dir, "probe")
	build := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", probe, "./testdata/qemu-probe")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	output, err := build.CombinedOutput()
	require.NoError(t, err, string(output))
	var boxes []*config.Sandbox
	for _, name := range []string{"first", "second", "third"} {
		sb := &config.Sandbox{Name: name, Provider: config.ProviderQEMU, CPU: 1, MemoryMiB: 512, MemoryMaxMiB: 768, DiskMiB: 1024, Image: "docker.io/library/alpine:3.20"}
		repo := filepath.Join(dir, "repo", name)
		wt := filepath.Join(s.WorktreeDir(sb.Key()), "work")
		require.NoError(t, os.MkdirAll(repo, 0700))
		require.NoError(t, os.MkdirAll(wt, 0700))
		require.NoError(t, os.WriteFile(filepath.Join(wt, "identity"), []byte(name), 0600))
		require.NoError(t, os.Link(probe, filepath.Join(wt, "probe")))
		sb.Phases = []config.Phase{{Repo: repo, Worktree: wt}}
		sb.Network.Use = []string{}
		sb.Network.Blocks = []config.NetworkBlock{{Origin: config.BlockOriginCustom, AllowIPs: []string{"127.0.0.1", "192.168.127.1"}}}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		port := listener.Addr().(*net.TCPAddr).Port
		require.NoError(t, listener.Close())
		sb.Forwards = []config.PortForward{{HostPort: port, GuestPort: 18080}}

		require.NoError(t, s.Save(sb))
		require.NoError(t, p.CreateContext(ctx, sb))
		boxes = append(boxes, sb)
		t.Cleanup(func() {
			if t.Failed() {
				b, _ := os.ReadFile(filepath.Join(s.VMDir(sb.Key()), "qemud.log"))
				t.Logf("%s daemon: %s", sb.Key(), b)
			}
			_ = p.Destroy(sb)
		})
		if name == "first" {
			short, done := context.WithTimeout(ctx, time.Second)
			err := p.StartContext(short, sb)
			done()
			require.ErrorIs(t, err, context.DeadlineExceeded)
			status, err := p.Status(sb)
			require.NoError(t, err)
			require.Equal(t, "stopped", status)
			require.NoFileExists(t, filepath.Join(p.QEMUStateDir(sb), "qemu.pid"))
			require.NoFileExists(t, filepath.Join(s.VMDir(sb.Key()), "qemu-daemon.pid"))
		}
		require.NoError(t, p.StartContext(ctx, sb))
	}
	for _, sb := range boxes {
		sock := filepath.Join(s.VMDir(sb.Key()), "agent.sock")
		out, code, err := vsockclient.Output(ctx, sock, 0, sandbox.GuestUser, "/bin/sh", "-c", "cat "+sandbox.WorkspaceRoot+"/work/identity; echo saved > /home/agent/.claude/identity")
		require.NoError(t, err)
		require.Zero(t, code, out)
		require.Equal(t, sb.Name, out)
		require.FileExists(t, filepath.Join(s.StateDir(sb.Key()), "claude", "identity"))
		ctl := vzdctl.NewClient(vzdctl.SocketPath(s.VMDir(sb.Key())))
		out, code, err = vsockclient.Output(ctx, sock, 0, sandbox.GuestUser, "/bin/sh", "-c", "wget -T 3 -qO- http://192.168.127.1:"+hostPort+"; "+sandbox.WorkspaceRoot+"/work/probe http-start "+sandbox.WorkspaceRoot+"/work")
		require.NoError(t, err)
		require.Zero(t, code, out)
		require.Equal(t, "host-egress", out)
		var response *http.Response
		require.Eventually(t, func() bool {
			response, err = (&http.Client{Timeout: 3 * time.Second}).Get(fmt.Sprintf("http://127.0.0.1:%d/identity", sb.Forwards[0].HostPort))
			return err == nil
		}, 5*time.Second, 100*time.Millisecond)
		require.NoError(t, err)
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		require.NoError(t, err)
		require.Equal(t, sb.Name, string(body))
		sb.Network.Blocks[0].DenyIPs = []string{"127.0.0.1", "192.168.127.1"}
		require.NoError(t, s.Save(sb))
		require.NoError(t, ctl.Reload(ctx))
		_, code, err = vsockclient.Output(ctx, sock, 0, sandbox.GuestUser, "/bin/sh", "-c", "if wget -T 2 -qO- http://192.168.127.1:"+hostPort+"; then exit 1; fi")
		require.NoError(t, err)
		require.Zero(t, code, "live policy reload must deny host egress")

		require.NoError(t, ctl.Pause(ctx))
		require.NoError(t, ctl.Resume(ctx))
		require.NoError(t, ctl.Suspend(ctx))
		require.Eventually(t, func() bool { status, _ := p.Status(sb); return status != "running" }, 20*time.Second, 50*time.Millisecond)
		require.NoError(t, p.StartContext(ctx, sb))
		state, err := ctl.Lifecycle(ctx)
		require.NoError(t, err)
		require.True(t, state.Restored)
		out, code, err = vsockclient.Output(ctx, sock, 0, sandbox.GuestUser, "cat", sandbox.WorkspaceRoot+"/work/identity")
		require.NoError(t, err)
		require.Zero(t, code, out)
		require.Equal(t, sb.Name, out)
	}
	require.NoError(t, p.Stop(boxes[1]))
	// Exercise the shared idle watchdog through the actual daemon.
	boxes[1].IdleTimeoutSec = 1
	boxes[1].DesiredState = config.VMStateRunning
	require.NoError(t, s.Save(boxes[1]))
	require.NoError(t, p.StartContext(ctx, boxes[1]))
	require.Eventually(t, func() bool {
		fresh, err := s.Load(boxes[1].Key())
		status, _ := p.Status(boxes[1])
		return err == nil && fresh.StopReason == config.StopReasonIdle && status == "stopped"
	}, 3*time.Minute, time.Second)
	require.NoError(t, p.Destroy(boxes[1]))
	require.NoError(t, s.Delete(boxes[1].Key()))
	for _, sb := range []*config.Sandbox{boxes[0], boxes[2]} {
		status, err := p.Status(sb)
		require.NoError(t, err)
		require.Equal(t, "running", status)
		require.True(t, s.Exists(sb.Key()))
		require.NoError(t, p.Destroy(sb))
	}
}
