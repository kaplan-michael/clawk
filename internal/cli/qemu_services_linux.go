//go:build linux && amd64

package cli

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/clawkwork/clawk/internal/vsockclient"
	"github.com/clawkwork/clawk/machine"
)

// qemuGuestServices owns QEMU's host service endpoints. Readiness uses
// a private agent socket; publishing the client socket is the final step.
type qemuGuestServices struct {
	admin, proxy *agentProxy
	reverse      *reverseProxy
	serial       *serialProxy
	ssh          *sshAgentProxy
}

func (s *qemuGuestServices) Close() {
	if s.proxy != nil {
		s.proxy.Stop()
	}
	if s.admin != nil {
		s.admin.Stop()
	}
	s.ssh.Stop()
	s.serial.Stop()
	s.reverse.Stop()
}

func startQEMUGuestServices(ctx, ready context.Context, m machine.Machine, sb *config.Sandbox, vmDir string, rev *reverseProxy, ser *serialProxy, logger *log.Logger) (_ *qemuGuestServices, err error) {
	s := &qemuGuestServices{reverse: rev, serial: ser}
	defer func() {
		if err != nil {
			s.Close()
		}
	}()
	if err = rev.Start(ctx, m); err != nil {
		return nil, fmt.Errorf("reverse forwarding: %w", err)
	}
	if err = ser.Start(ctx, m); err != nil {
		return nil, fmt.Errorf("serial forwarding: %w", err)
	}
	if s.ssh, err = startSSHAgentProxy(ctx, m, logger); err != nil {
		return nil, err
	}
	sock := filepath.Join(vmDir, "agent-init.sock")
	if s.admin, err = startAgentProxy(ctx, m, sock, logger); err != nil {
		return nil, err
	}
	if err = waitGuestAgent(ready, sock); err != nil {
		return nil, err
	}
	if err = verifyGuestIdentity(ready, sock); err != nil {
		return nil, err
	}
	if err = verifyGuestMounts(ready, sock, sb); err != nil {
		return nil, err
	}
	if err = waitGuestReverseForwards(ready, sock, sb); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *qemuGuestServices) Publish(ctx context.Context, m machine.Machine, sb *config.Sandbox, vmDir string, lc *vmLifecycle, logger *log.Logger) error {
	var err error
	s.proxy, err = startAgentProxy(ctx, m, filepath.Join(vmDir, "agent.sock"), logger)
	if err != nil {
		return err
	}
	startIdleWatchdog(ctx, sb, m, s.proxy, logger)
	startWallclockWatchdog(ctx, m, s.proxy, lc.isUserPaused, logger)
	startTimeSyncSender(ctx, m, logger)
	return nil
}

func verifyGuestMounts(ctx context.Context, sock string, sb *config.Sandbox) error {
	manifest, err := sandbox.QEMUGuestManifest(sb, store)
	if err != nil {
		return err
	}
	var script strings.Builder
	script.WriteString("set -e\n")
	for _, mount := range manifest.Mounts {
		fmt.Fprintf(&script, "mountpoint -q %s && test -d %s || { echo %s; exit 1; }\n", shellQuote(mount.Path), shellQuote(mount.Path), shellQuote("shared directory unavailable: "+mount.Path))
	}
	check, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, code, err := vsockclient.Output(check, sock, 0, "root", "/bin/sh", "-c", script.String())
	if err != nil {
		return fmt.Errorf("guest mount readiness: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("guest mount readiness: %s", strings.TrimSpace(out))
	}
	return nil
}

// Restored vsock subscriptions reconnect asynchronously. Wait for the
// requested guest listeners before exposing agent.sock, so the first user
// command can use a reverse forward immediately after startup/restore.
func waitGuestReverseForwards(ctx context.Context, sock string, sb *config.Sandbox) error {
	if len(sb.ReverseForwards) == 0 {
		return nil
	}
	var script strings.Builder
	script.WriteString("set -e\n")
	for _, f := range sb.ReverseForwards {
		fmt.Fprintf(&script, "awk '$4 == \"0A\" { split($2,a,\":\"); if (a[2] == \"%04X\") found=1 } END { exit !found }' /proc/net/tcp /proc/net/tcp6\n", f.GuestPort)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for {
		_, code, err := vsockclient.Output(ctx, sock, 0, "root", "/bin/sh", "-c", script.String())
		if err == nil && code == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("guest reverse-forward listeners did not become ready: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Readiness is read-only: PID 1 owns account and sudo configuration.
func verifyGuestIdentity(ctx context.Context, sock string) error {
	script := fmt.Sprintf("test \"$(id -un)\" = %s && test \"$(id -u)\" = %d && test \"$(id -g)\" = %d && { ! command -v sudo >/dev/null 2>&1 || sudo -n true; }", shellQuote(sandbox.GuestUser), os.Getuid(), os.Getgid())
	out, code, err := vsockclient.Output(ctx, sock, 0, sandbox.GuestUser, "/bin/sh", "-c", script)
	if err != nil {
		return fmt.Errorf("guest identity readiness: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("guest identity/passwordless sudo verification exited %d: %s", code, strings.TrimSpace(out))
	}
	return nil
}

func waitGuestAgent(ctx context.Context, path string) error {
	for {
		if err := vsockclient.Ping(ctx, path, 0, 3*time.Second); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
