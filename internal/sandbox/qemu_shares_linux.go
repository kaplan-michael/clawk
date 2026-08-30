//go:build linux && amd64

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/guestcfg"
)

// qemuWorktreeShares checks recorded sources before exporting them. Managed
// worktrees under the store's existing parent share one virtio-fs device;
// recorded paths elsewhere keep individual exports. Missing paths are errors.
func qemuWorktreeShares(sb *config.Sandbox, parent string) ([]HostShare, error) {
	var out []HostShare
	consolidated := false
	seenPaths := map[string]bool{}
	seenRepos := map[string]bool{}
	destinations := map[string]string{}
	for _, p := range sb.Phases {
		if p.Worktree == "" {
			continue
		}
		path := filepath.Clean(p.Worktree)
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("recorded worktree %s: %w", path, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("recorded worktree %s is not a directory", path)
		}
		dest := filepath.Join(WorkspaceRoot, filepath.Base(path))
		if previous, ok := destinations[dest]; ok && previous != path {
			return nil, fmt.Errorf("worktrees %s and %s have the same guest mount %s", previous, path, dest)
		}
		destinations[dest] = path
		if !seenPaths[path] {
			seenPaths[path] = true
			if !p.InPlace && filepath.Dir(path) == parent {
				consolidated = true
			} else {
				out = append(out, HostShare{HostPath: path, GuestPath: dest, Tag: InPlaceWorktreeTag(path)})
			}
		}
		if !p.InPlace && !seenRepos[p.Repo] {
			seenRepos[p.Repo] = true
			info, err := os.Stat(p.Repo)
			if err != nil {
				return nil, fmt.Errorf("recorded repository %s: %w", p.Repo, err)
			}
			if !info.IsDir() {
				return nil, fmt.Errorf("recorded repository %s is not a directory", p.Repo)
			}
			out = append(out, HostShare{HostPath: p.Repo, GuestPath: p.Repo, Tag: RepoShareTag(p.Repo)})
		}
	}
	if consolidated {
		out = append([]HostShare{{HostPath: parent, GuestPath: WorkspaceRoot, Tag: WorkspaceShareTag}}, out...)
	}
	return out, nil
}

// QEMUShares enumerates QEMU exports in guest mount order. QEMU cold boots
// replace the rootfs, so every persistent state directory must be prepared;
// silently dropping one would lose agent state on the next boot.
func QEMUShares(sb *config.Sandbox, store *config.Store) ([]HostShare, error) {
	shares, err := qemuWorktreeShares(sb, store.WorktreeDir(sb.Key()))
	if err != nil {
		return nil, err
	}
	stateDir := store.StateDir(sb.Key())
	for _, dir := range AgentStateDirs {
		if err := os.MkdirAll(filepath.Join(stateDir, dir.Sub), 0o755); err != nil {
			return nil, fmt.Errorf("preparing persistent %s state: %w", dir.Agent, err)
		}
	}
	shares = append(shares, PersistentAgentShares(stateDir)...)
	shares = append(shares, DefaultHostShares()...)
	shares = append(shares, ToolchainCacheShares(store.CacheDir())...)
	shares = append(shares, UserHostShares(sb.Shares)...)
	for _, sh := range shares {
		info, err := os.Stat(sh.HostPath)
		if err != nil {
			return nil, fmt.Errorf("QEMU export %s (%s): %w", sh.Tag, sh.HostPath, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("QEMU export %s (%s) is not a directory", sh.Tag, sh.HostPath)
		}
	}
	return shares, nil
}

// QEMUGuestManifest uses the existing manifest schema, replacing only the
// mount list with QEMU's checked exports. Other providers retain their own
// share enumeration and best-effort failure policy.
func QEMUGuestManifest(sb *config.Sandbox, store *config.Store) (guestcfg.Manifest, error) {
	shares, err := QEMUShares(sb, store)
	if err != nil {
		return guestcfg.Manifest{}, err
	}
	m, err := OCIGuestManifest(sb, store.StateDir(sb.Key()), store.CacheDir(), store.RootDir())
	if err != nil {
		return guestcfg.Manifest{}, err
	}
	m.Mounts = nil
	for _, sh := range shares {
		// QEMU exposes all shares through virtiofsd, including caches if enabled.
		m.Mounts = append(m.Mounts, guestcfg.Mount{Tag: sh.Tag, Path: sh.GuestPath, ReadOnly: sh.ReadOnly})
	}
	return m, nil
}
