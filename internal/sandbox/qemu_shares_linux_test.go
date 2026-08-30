//go:build linux && amd64

package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/stretchr/testify/require"
)

func TestQEMUSharesRecordedPaths(t *testing.T) {
	store := config.NewStoreAt(t.TempDir())
	root := store.RootDir()
	repo := filepath.Join(root, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	sb := &config.Sandbox{Name: "box", Namespace: config.DefaultNamespace}
	canonical := store.WorktreeDir(sb.Key())
	managed := filepath.Join(canonical, "repo")
	legacy := filepath.Join(root, "old", sb.NamespaceName(), "legacy")
	inplace := filepath.Join(root, "here")
	for _, p := range []string{managed, legacy, inplace} {
		require.NoError(t, os.MkdirAll(p, 0o755))
	}
	marker := filepath.Join(legacy, "saved.txt")
	require.NoError(t, os.WriteFile(marker, []byte("user data"), 0o600))
	sb.Phases = []config.Phase{{Repo: repo, Worktree: managed}, {Repo: repo, Worktree: legacy}, {Repo: inplace, Worktree: inplace, InPlace: true}}
	shares, err := QEMUShares(sb, store)
	require.NoError(t, err)
	require.Equal(t, HostShare{HostPath: canonical, GuestPath: WorkspaceRoot, Tag: WorkspaceShareTag}, shares[0])
	require.Contains(t, shares, HostShare{HostPath: legacy, GuestPath: WorkspaceRoot + "/legacy", Tag: InPlaceWorktreeTag(legacy)})
	m, err := QEMUGuestManifest(sb, store)
	require.NoError(t, err)
	for i, share := range shares {
		require.Equal(t, share.Tag, m.Mounts[i].Tag)
		require.Equal(t, share.GuestPath, m.Mounts[i].Path)
	}
	require.FileExists(t, marker)
	require.NoError(t, os.RemoveAll(legacy))
	_, err = QEMUShares(sb, store)
	require.ErrorContains(t, err, "recorded worktree")
	require.NoDirExists(t, legacy)
}

func TestQEMUSharesRejectMissingRequiredStorage(t *testing.T) {
	store := config.NewStoreAt(t.TempDir())
	sb := &config.Sandbox{Name: "box"}
	stateDir := store.StateDir(sb.Key())
	require.NoError(t, os.MkdirAll(stateDir, 0o755))
	blocked := filepath.Join(stateDir, AgentStateDirs[0].Sub)
	require.NoError(t, os.WriteFile(blocked, []byte("occupied"), 0o600))
	_, err := QEMUShares(sb, store)
	require.ErrorContains(t, err, "preparing persistent")
	_, err = QEMUGuestManifest(sb, store)
	require.ErrorContains(t, err, "preparing persistent")
	require.NoError(t, os.Remove(blocked))

	missing := filepath.Join(store.RootDir(), "missing")
	sb.Shares = []config.HostShare{{HostPath: missing, GuestPath: "/required"}}
	_, err = QEMUShares(sb, store)
	require.ErrorContains(t, err, "QEMU export")
	require.NoDirExists(t, missing)
	require.NoError(t, os.WriteFile(missing, nil, 0o600))
	_, err = QEMUShares(sb, store)
	require.ErrorContains(t, err, "not a directory")
}
