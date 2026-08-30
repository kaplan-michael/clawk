//go:build linux && amd64

package qemu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/clawkwork/clawk/machine"
	"github.com/clawkwork/clawk/machine/internal/cow"
	"github.com/clawkwork/clawk/machine/oci"
)

// MaterializeRootFS prepares a disposable QEMU disk atomically. A restore
// source that aliases the target is reused without truncating the saved disk.
func MaterializeRootFS(ctx context.Context, r machine.RootFS, dstPath string) (machine.RawDisk, error) {
	if err := ctx.Err(); err != nil {
		return machine.RawDisk{}, err
	}
	src, err := oci.Resolve(ctx, r)
	if err != nil {
		return machine.RawDisk{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return machine.RawDisk{}, fmt.Errorf("qemu: preparing disk dir: %w", err)
	}
	srcInfo, err := os.Stat(src.Path)
	if err != nil {
		return machine.RawDisk{}, err
	}
	if dstInfo, err := os.Stat(dstPath); err == nil && os.SameFile(srcInfo, dstInfo) {
		return machine.RawDisk{Path: dstPath}, nil
	}
	// A killed creator must not leave a partial disk that the next boot
	// mistakes for a persistent, previously initialized root filesystem.
	tmp, err := os.CreateTemp(filepath.Dir(dstPath), ".disk-*")
	if err != nil {
		return machine.RawDisk{}, err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Close(); err != nil {
		return machine.RawDisk{}, err
	}
	if err := cow.Clone(src.Path, name); err != nil {
		return machine.RawDisk{}, fmt.Errorf("qemu: cloning %s → %s: %w",
			src.Path, dstPath, err)
	}
	if err := ctx.Err(); err != nil {
		return machine.RawDisk{}, err
	}
	if err := os.Rename(name, dstPath); err != nil {
		return machine.RawDisk{}, err
	}
	return machine.RawDisk{Path: dstPath}, nil
}
