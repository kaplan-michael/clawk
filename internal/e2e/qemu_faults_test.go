//go:build linux && amd64

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/clawkwork/clawk/machine"
	"github.com/stretchr/testify/require"
)

func runQEMUFaultChecks(t *testing.T, ctx context.Context, m machine.Machine, dir, share string, run func(string) string) {
	t.Helper()
	snapshot := m.(machine.Snapshottable)
	bad := filepath.Join(dir, "not-a-directory")
	require.NoError(t, os.WriteFile(bad, []byte("occupied"), 0o600))
	require.Error(t, snapshot.Snapshot(ctx, bad))
	require.Contains(t, run("cat /home/agent/disk-proof"), "retained", "failed snapshot must leave the guest usable")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	require.Error(t, snapshot.Snapshot(cancelled, filepath.Join(dir, "cancelled")))
	require.Contains(t, run("cat /home/agent/disk-proof"), "retained")
	if fullFS := os.Getenv("TEST_QEMU_SMALL_FS"); fullFS != "" {
		// The harness supplies a private 1 MiB tmpfs to force a real write
		// failure partway through migration, without filling a host disk.
		dst, err := os.MkdirTemp(fullFS, "snapshot-")
		require.NoError(t, err)
		defer os.RemoveAll(dst)
		bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
		err = snapshot.Snapshot(bounded, dst)
		cancel()
		require.ErrorIs(t, err, syscall.ENOSPC)
		require.NotErrorIs(t, err, context.DeadlineExceeded, "disk-full must fail promptly")
		t.Logf("disk-full snapshot failed promptly: %v", err)
		require.NoFileExists(t, filepath.Join(dst, "qemu.state"))
		require.NoFileExists(t, filepath.Join(dst, "qemu.state.partial"))
		require.Contains(t, run("cat /home/agent/disk-proof"), "retained")
	}

	// A truncated stream must clean up its incoming VM and allow a cold boot
	// using the same persistent disk and current host files.
	save := filepath.Join(dir, "truncated")
	run("sync")
	require.NoError(t, m.(machine.Suspendable).Suspend(ctx, save))
	require.NoError(t, os.Truncate(filepath.Join(save, "qemu.state"), 16))
	restore, done := context.WithTimeout(ctx, 15*time.Second)
	err := snapshot.Restore(restore, save)
	done()
	require.Error(t, err)
	require.NoError(t, m.Start(ctx))
	require.Contains(t, run("cat /home/agent/disk-proof"), "retained")

	for _, executable := range []string{"qemu-system-", "virtiofsd"} {
		pids := qemuFixtureProcesses(t, dir, executable)
		require.NotEmpty(t, pids, executable)
		require.NoError(t, syscall.Kill(pids[0], syscall.SIGKILL))
		require.Eventually(t, func() bool {
			state, _ := m.State(ctx)
			return state == machine.StateStopped
		}, 5*time.Second, 25*time.Millisecond, "child exit must be observable")
		require.NoError(t, m.Start(ctx), "restart after %s crash", executable)
		require.Contains(t, run("cat /home/agent/disk-proof"), "retained")
		require.Len(t, qemuFixtureProcesses(t, dir, "qemu-system-"), 1)
	}

	cycles := 10
	if n, err := strconv.Atoi(os.Getenv("TEST_QEMU_STRESS_CYCLES")); err == nil && n > 0 {
		cycles = n
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < cycles; i++ {
				for _, op := range []func(context.Context) error{m.(machine.Pauseable).Pause, m.(machine.Pauseable).Resume} {
					if err := op(ctx); err != nil && !errors.Is(err, machine.ErrInvalidState) {
						errs <- err
						return
					}
				}
				_, _ = m.State(ctx)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	_ = m.(machine.Pauseable).Resume(ctx)
	require.Contains(t, run("cat /workspace/host.txt; cat /home/agent/disk-proof"), "retained")
	require.NoError(t, m.Stop(ctx, false))
	require.Empty(t, qemuFixtureProcesses(t, dir, ""), "QEMU and all export helpers must be reaped")
	// Starting concurrently must launch exactly one machine and helper set.
	startErrors := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); startErrors <- m.Start(ctx) }()
	}
	wg.Wait()
	close(startErrors)
	for err := range startErrors {
		require.NoError(t, err)
	}
	require.Len(t, qemuFixtureProcesses(t, dir, "qemu-system-"), 1)
	require.Contains(t, run("cat /home/agent/disk-proof"), "retained")
	t.Logf("fault checks passed: snapshot errors, truncated restore, QEMU/helper crashes, %d concurrent pause/resume operations and concurrent starts", 8*cycles)
	// Destroy must not drop the lifecycle lock between stopping children and
	// deleting their state: a queued Start could otherwise launch an orphan.
	destroyed := make(chan error, 1)
	go func() { destroyed <- m.Destroy(ctx) }()
	time.Sleep(5 * time.Millisecond)
	err = m.Start(ctx)
	if err != nil {
		require.ErrorIs(t, err, machine.ErrInvalidState)
	}
	require.NoError(t, <-destroyed)
	state, err := m.State(ctx)
	require.NoError(t, err)
	require.Equal(t, machine.StateDestroyed, state)
	require.Empty(t, qemuFixtureProcesses(t, dir, ""))
	require.NoDirExists(t, dir)
}

func qemuFixtureProcesses(t *testing.T, dir, executable string) []int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	require.NoError(t, err)
	var result []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil || len(b) == 0 {
			continue
		}
		args := strings.Split(string(b), "\x00")
		if (strings.Contains(args[0], "qemu-system-") || strings.HasSuffix(args[0], "/virtiofsd")) && strings.Contains(args[0], executable) && strings.Contains(string(b), dir+"/") {
			result = append(result, pid)
		}
	}
	return result
}
