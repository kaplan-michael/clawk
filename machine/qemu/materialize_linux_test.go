//go:build linux && amd64

package qemu

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/clawkwork/clawk/machine"
	"github.com/stretchr/testify/require"
)

func TestMaterializePreservesDiskOnFailure(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "disk")
	require.NoError(t, os.WriteFile(src, []byte("new disk"), 0600))
	require.NoError(t, os.WriteFile(dst, []byte("existing disk"), 0600))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := MaterializeRootFS(ctx, machine.RawDisk{Path: src}, dst)
	require.ErrorIs(t, err, context.Canceled)
	data, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Equal(t, "existing disk", string(data))
	partials, err := filepath.Glob(filepath.Join(dir, ".disk-*"))
	require.NoError(t, err)
	require.Empty(t, partials)
	_, err = MaterializeRootFS(context.Background(), machine.RawDisk{Path: src}, dst)
	require.NoError(t, err)
	data, err = os.ReadFile(dst)
	require.NoError(t, err)
	require.Equal(t, "new disk", string(data))
}

func TestMaterializeAliasDoesNotTruncateSource(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "alias")
	require.NoError(t, os.WriteFile(src, []byte("persistent"), 0600))
	require.NoError(t, os.Link(src, dst))
	_, err := MaterializeRootFS(context.Background(), machine.RawDisk{Path: src}, dst)
	require.NoError(t, err)
	b, err := os.ReadFile(src)
	require.NoError(t, err)
	require.Equal(t, "persistent", string(b))
}
