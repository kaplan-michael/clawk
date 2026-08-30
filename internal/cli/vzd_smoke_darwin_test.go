//go:build darwin

package cli

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/clawkwork/clawk/internal/vsockclient"
	"github.com/clawkwork/clawk/machine"
	"github.com/clawkwork/clawk/machine/balloon"
	"github.com/clawkwork/clawk/machine/guest"
	"github.com/clawkwork/clawk/machine/vz"
	"github.com/stretchr/testify/require"
)

// Run the signed test binary on native macOS; see scripts/test-vz-macos.sh.
func TestVZSharedServices(t *testing.T) {
	if os.Getenv("TEST_VZ_BOOT") != "1" {
		t.Skip("set TEST_VZ_BOOT=1 and sign the test binary")
	}
	withTempStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if cache := os.Getenv("TEST_GUEST_BOOT_CACHE"); cache != "" {
		require.NoError(t, os.Symlink(cache, store.CacheDir()))
	}
	repo := filepath.Join(store.RootDir(), "repo")
	require.NoError(t, os.MkdirAll(repo, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "host.txt"), []byte("live share"), 0600))
	sb := &config.Sandbox{Name: "box", Provider: config.ProviderVZ, CPU: 2, MemoryMiB: 1024, MemoryMaxMiB: 1536, DiskMiB: 8192, Image: defaultImage, Phases: []config.Phase{{Repo: repo, Worktree: repo, InPlace: true}}}
	require.NoError(t, store.Save(sb))
	p := sandbox.NewVZProvider(store)
	require.NoError(t, p.Create(sb))
	logger := log.New(io.Discard, "", 0)
	allow, err := startAllowList(sb, logger)
	require.NoError(t, err)
	defer allow.Stop()
	vmDir := store.VMDir(sb.Name)
	spec, err := buildOCISandboxSpec(sb, vmDir, allow)
	require.NoError(t, err)
	backend, err := machine.Get("vz")
	require.NoError(t, err)
	m, err := backend.New(ctx, spec, vmDir)
	require.NoError(t, err)
	defer func() {
		stop, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		_ = m.Stop(stop, false)
	}()
	require.NoError(t, m.Create(ctx))
	require.NoError(t, m.Start(ctx))
	lc := newVMLifecycle(sb.Name, vmDir, logger)
	lc.attach(m, false)
	rev, ser := newReverseProxy(logger), newSerialProxy(logger)
	sock := filepath.Join(vmDir, "agent.sock")
	proxy, err := startAgentProxy(ctx, m, sock, logger)
	require.NoError(t, err)
	defer proxy.Stop()
	ssh, err := startSSHAgentProxy(ctx, m, logger)
	require.NoError(t, err)
	defer ssh.Stop()
	require.NoError(t, rev.Start(ctx, m))
	defer rev.Stop()
	require.NoError(t, ser.Start(ctx, m))
	defer ser.Stop()
	startWallclockWatchdog(ctx, m, proxy, lc.isUserPaused, logger)
	startIdleWatchdog(ctx, sb, m, proxy, logger)
	startTimeSyncSender(ctx, m, logger)
	require.Eventually(t, func() bool {
		return vsockclient.Ping(ctx, sock, 0, time.Second) == nil
	}, time.Minute, 100*time.Millisecond)
	check := func() {
		out, code, err := vsockclient.Output(ctx, sock, 0, sandbox.GuestUser, "/bin/sh", "-c", "set -eu; sudo -n visudo -c; cat "+sandbox.WorkspaceRoot+"/repo/host.txt; echo guest > "+sandbox.WorkspaceRoot+"/repo/guest.txt")
		require.NoError(t, err)
		require.Zero(t, code)
		require.Contains(t, out, "live share")
		stats, err := guest.FetchStats(ctx, m)
		require.NoError(t, err)
		require.Positive(t, stats.TotalKiB)
		require.True(t, stats.HasActivity)
		compatible, err := vz.FetchGuestStats(ctx, m)
		require.NoError(t, err)
		require.True(t, compatible.HasActivity)
		var policy balloon.Policy
		target := policy.Target(768<<20, 512<<20, 768<<20, 0, stats)
		require.GreaterOrEqual(t, target, uint64(512<<20))
		require.LessOrEqual(t, target, uint64(768<<20))
	}
	check()
	require.NoError(t, m.(machine.Pauseable).Pause(ctx))
	require.NoError(t, m.(machine.Pauseable).Resume(ctx))
	check()
	require.FileExists(t, filepath.Join(repo, "guest.txt"))
}
