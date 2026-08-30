//go:build linux && amd64

package qemu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type HostCheck struct {
	Name, Detail, Fix string
	Err               error
	Optional          bool
}

func BinaryName() string { return "qemu-system-x86_64" }

const qemuVersionRequirement = "QEMU >= 10.1"

// Doctor and direct machine-library callers must enforce the same minimum.
func checkQEMUVersion(output string) error {
	if !versionAtLeast(output, 10, 1) {
		return fmt.Errorf("%s is required; found %s", qemuVersionRequirement, strings.TrimSpace(output))
	}
	return nil
}

// FindVirtiofsd uses the same executable lookup for diagnostics and launch.
func FindVirtiofsd() (string, error) {
	for _, p := range []string{"virtiofsd", "/usr/libexec/virtiofsd", "/usr/lib/qemu/virtiofsd", "/usr/lib/virtiofsd"} {
		if x, err := exec.LookPath(p); err == nil {
			return x, nil
		}
	}
	return "", errors.New("virtiofsd not executable on PATH or in standard libexec locations")
}

func packageHint() string {
	b, _ := os.ReadFile("/etc/os-release")
	s := string(b)
	switch {
	case strings.Contains(s, "fedora"), strings.Contains(s, "rhel"):
		return "sudo dnf install qemu-system-x86-core virtiofsd"
	case strings.Contains(s, "debian"), strings.Contains(s, "ubuntu"):
		return "sudo apt install qemu-system-x86 virtiofsd; " + qemuVersionRequirement + ", virtiofsd >= 1.14"
	case strings.Contains(s, "arch"):
		return "sudo pacman -S qemu-system-x86 virtiofsd"
	default:
		return "install " + qemuVersionRequirement + " and virtiofsd >= 1.14 using your distribution package manager"
	}
}

func probeOutput(ctx context.Context, binary string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", binary, err, strings.TrimSpace(string(b)))
	}
	return string(b), nil
}

func versionAtLeast(output string, major, minor int) bool {
	for _, word := range strings.Fields(output) {
		var a, b int
		if _, err := fmt.Sscanf(strings.TrimPrefix(word, "v"), "%d.%d", &a, &b); err == nil {
			return a > major || a == major && b >= minor
		}
	}
	return false
}

// CheckHost only probes local prerequisites. It never installs packages or
// changes device permissions. Foreground preflight and doctor share it.
func CheckHost(ctx context.Context) []HostCheck {
	var checks []HostCheck
	add := func(name, detail, fix string, err error) {
		checks = append(checks, HostCheck{Name: name, Detail: detail, Fix: fix, Err: err})
	}
	add("architecture", runtime.GOARCH, "use a Linux amd64 (x86-64) host", nil)
	var rootErr error
	if os.Geteuid() == 0 {
		rootErr = errors.New("QEMU refuses root execution")
	}
	add("user", "unprivileged", "run clawk as your regular user", rootErr)
	for _, path := range []string{"/dev/kvm", "/dev/vhost-vsock"} {
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err == nil {
			_ = f.Close()
		}
		add(path, "readable/writable", "enable KVM and vhost_vsock; grant your user read/write access through your distribution's device groups/udev rules, then log in again", err)
	}
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err == nil {
		_ = unix.Close(fd)
	}
	add("AF_VSOCK", "available", "load the vsock and vhost_vsock kernel modules", err)
	q, err := exec.LookPath(BinaryName())
	if err == nil {
		var out string
		out, err = probeOutput(ctx, q, "--version")
		if err == nil {
			err = checkQEMUVersion(out)
		}
	}
	add("qemu", q, packageHint(), err)
	if err == nil {
		out, e := probeOutput(ctx, q, "-device", "help")
		for _, device := range []string{"virtio-blk-device", "virtio-net-device", "vhost-vsock-device", "vhost-user-fs-pci", "virtio-balloon-device", "virtio-rng-device"} {
			if e == nil && !strings.Contains(out, device) {
				e = fmt.Errorf("QEMU build is missing %s", device)
			}
		}
		add("qemu devices", "required virtio devices available", packageHint(), e)
	}
	v, err := FindVirtiofsd()
	if err == nil {
		out, e := probeOutput(ctx, v, "--version")
		err = e
		if err == nil && !versionAtLeast(out, 1, 14) {
			err = errors.New("virtiofsd >= 1.14 is required")
		}
	}
	add("virtiofsd", v, packageHint(), err)
	checks = append(checks, runtimeHostChecks(ctx, q, v)...)
	return checks
}

func Preflight(ctx context.Context) error {
	var failures []error
	for _, c := range CheckHost(ctx) {
		if c.Err != nil && !c.Optional {
			failures = append(failures, fmt.Errorf("%s: %w; %s", c.Name, c.Err, c.Fix))
		}
	}
	return errors.Join(failures...)
}
