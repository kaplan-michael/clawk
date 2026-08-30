//go:build linux && amd64

package qemu

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// HDA's controller and codec have migration state in QEMU 10.2. The
// virtio-sound device is unmigratable and would disable sandbox suspend.
func audioArgs(backend string) []string {
	if backend == "" {
		return nil
	}
	return []string{"-audiodev", backend + ",id=audio0", "-device", "intel-hda,id=hda0,msi=on", "-device", "hda-micro,bus=hda0.0,audiodev=audio0"}
}

func audioBackend(ctx context.Context, binary string) string {
	if os.Getenv("CLAWK_AUDIO_INPUT") == "0" {
		return ""
	}
	devices, err := probeOutput(ctx, binary, "-device", "help")
	if err != nil || !strings.Contains(devices, `name "intel-hda"`) || !strings.Contains(devices, `name "hda-micro"`) {
		return ""
	}
	drivers, err := probeOutput(ctx, binary, "-audio", "driver=help")
	if err != nil {
		return ""
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return ""
	}
	if strings.Contains(drivers, "pipewire") {
		if _, err := os.Stat(filepath.Join(dir, "pipewire-0")); err == nil {
			return "pipewire"
		}
	}
	if strings.Contains(drivers, "pa") {
		socket := filepath.Join(dir, "pulse/native")
		if _, err := os.Stat(socket); err == nil {
			// QEMU's implicit Pulse server path additionally requires a
			// pulse/pid file, which PipeWire and private sessions may omit.
			return "pa,server=" + qemuValue("unix:"+socket)
		}
	}
	return ""
}
