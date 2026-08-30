//go:build linux && amd64

package kernel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// FetchQEMU selects the supported default kernel or an explicit override.
// Audio capture and nested KVM require a custom kernel.
func FetchQEMU(ctx context.Context, cacheDir, arch, override string) (string, error) {
	if arch != "amd64" {
		return "", fmt.Errorf("kernel: unsupported QEMU architecture %q", arch)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if override != "" {
		return Fetch(ctx, Options{CacheDir: cacheDir, Arch: arch, Override: override})
	}
	return FetchQEMUAMD64(ctx, cacheDir)
}

// DefaultQEMUBinaryPath is the x86 Linux boot-protocol image in the Kata
// archive. QEMU's x86 -kernel loader requires this bzImage-format member;
// the ELF vmlinux used by Firecracker and Virtualization.framework falls
// through to firmware instead of direct boot.
const DefaultQEMUBinaryPath = "opt/kata/share/kata-containers/vmlinuz-6.18.15-186"

// FetchQEMUAMD64 returns Kata's compressed x86 boot-protocol kernel image.
// It deliberately has a distinct cache path from Fetch's ELF vmlinux.
func FetchQEMUAMD64(ctx context.Context, cacheDir string) (string, error) {
	if cacheDir == "" {
		return "", errors.New("kernel: cache dir is required")
	}
	dst := filepath.Join(cacheDir, "kernels", fmt.Sprintf("kata-%s-amd64", DefaultKataVersion), "vmlinuz")
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	url := fmt.Sprintf("https://github.com/kata-containers/kata-containers/releases/download/%[1]s/kata-static-%[1]s-amd64.tar.zst", DefaultKataVersion)
	if err := download(ctx, url, DefaultQEMUBinaryPath, dst); err != nil {
		return "", err
	}
	return dst, nil
}
