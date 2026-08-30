//go:build linux && amd64

package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	qemubackend "github.com/clawkwork/clawk/machine/qemu"
	"github.com/stretchr/testify/require"
)

func TestQEMUOperationLockAndGuestUpgrade(t *testing.T) {
	p := NewQEMUProvider(config.NewStoreAt(t.TempDir()))
	sb := &config.Sandbox{Name: "box"}
	unlock, err := p.operationLock(sb)
	require.NoError(t, err)
	_, err = p.operationLock(sb)
	require.ErrorContains(t, err, "in progress")
	unlock()
	unlock, err = p.operationLock(sb)
	require.NoError(t, err)
	defer unlock()
	dir := p.vmDir(sb)
	require.NoError(t, CheckQEMUGuest(dir))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "disk.raw"), nil, 0o600))
	require.ErrorContains(t, CheckQEMUGuest(dir), "predates")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "qemu-guest-version"), []byte("3\n"), 0o600))
	require.ErrorContains(t, CheckQEMUGuest(dir), "save guest-only files")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "qemu-guest-version"), []byte(qemuGuestVersion), 0o600))
	require.NoError(t, CheckQEMUGuest(dir))
}

func TestQEMUStartupPhaseBudgets(t *testing.T) {
	budgets := qemubackend.StartupBudgets{Kernel: 10 * time.Millisecond, Preparation: 10 * time.Millisecond, Migration: 150 * time.Millisecond, GuestReady: 20 * time.Millisecond, Cleanup: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), budgets.Total(true))
	defer cancel()
	started := time.Now()
	require.NoError(t, waitQEMUStartup(ctx, make(chan error), func(ctx context.Context) error {
		select {
		case <-time.After(100 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}))
	require.Greater(t, time.Since(started), budgets.Total(false), "a valid restore can exceed the cold-start budget")
}

func TestQEMUStartupCancellationAndExit(t *testing.T) {
	for _, exit := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		exited := make(chan error, 1)
		probeDone := make(chan struct{})
		started := make(chan struct{})
		result := make(chan error, 1)
		go func() {
			result <- waitQEMUStartup(ctx, exited, func(ctx context.Context) error { close(started); <-ctx.Done(); close(probeDone); return ctx.Err() })
		}()
		<-started
		if exit {
			exited <- errors.New("child failed")
		} else {
			cancel()
		}
		select {
		case err := <-result:
			if exit {
				require.ErrorContains(t, err, "child failed")
			} else {
				require.ErrorIs(t, err, context.Canceled)
			}
		case <-time.After(time.Second):
			t.Fatal("startup did not react immediately")
		}
		select {
		case <-probeDone:
		case <-time.After(time.Second):
			t.Fatal("readiness probe leaked")
		}
		cancel()
	}
}

func TestQEMUPreparationAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := NewQEMUProvider(config.NewStoreAt(t.TempDir()))
	sb := &config.Sandbox{Name: "box", Namespace: "other"}
	require.ErrorIs(t, p.CreateContext(ctx, sb), context.Canceled)
	require.ErrorIs(t, p.StartContext(ctx, sb), context.Canceled)
	require.NoDirExists(t, p.vmDir(sb))
}
