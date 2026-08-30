//go:build linux && amd64

package qemu

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func runtimeHostChecks(ctx context.Context, binary, virtiofsd string) []HostCheck {
	var checks []HostCheck
	if binary != "" {
		audio := audioBackend(ctx, binary)
		err := probeQEMU(ctx, binary, audio)
		checks = append(checks, HostCheck{Name: "QEMU KVM/QMP/seccomp", Detail: "machine initialization and lifecycle commands available", Err: err, Fix: "install a " + qemuVersionRequirement + " build with KVM and seccomp support; check /dev/kvm permissions"})
		c := HostCheck{Name: "microphone", Detail: "disabled by CLAWK_AUDIO_INPUT=0", Optional: true}
		if os.Getenv("CLAWK_AUDIO_INPUT") != "0" {
			c.Detail = audio + "; HDA microphone (guest CONFIG_SND_HDA_INTEL/CONFIG_SND_HDA_GENERIC required)"
			if audio == "" {
				c.Err = fmt.Errorf("no supported PipeWire/PulseAudio capture backend detected")
				c.Fix = "install QEMU HDA/PipeWire or PulseAudio support and run in your desktop audio session; use a guest kernel with CONFIG_SND_HDA_INTEL=y and CONFIG_SND_HDA_GENERIC=y"
			}
		}
		checks = append(checks, c)
	}
	if virtiofsd != "" {
		err := probeVirtiofsd(ctx, virtiofsd)
		checks = append(checks, HostCheck{Name: "virtiofsd namespace/seccomp", Detail: "rootless sandbox starts with migration support", Err: err, Fix: "permit unprivileged user/mount namespaces for virtiofsd (including distribution AppArmor policy); install virtiofsd >= 1.14"})
	}
	if !nestedSupported() {
		checks = append(checks, HostCheck{Name: "nested virtualization", Optional: true, Err: fmt.Errorf("host KVM does not expose nested virtualization"), Fix: "enable nesting in the host hypervisor if needed; ordinary sandboxes work without it"})
	}
	return checks
}

func probeQEMU(ctx context.Context, binary, audio string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	args := []string{"-machine", machineOptions(), "-cpu", cpuModel(false), "-m", "64", "-S", "-nodefaults", "-no-user-config", "-display", "none", "-qmp", "stdio", "-only-migratable", "-sandbox", "on,elevateprivileges=deny,spawn=deny,obsolete=deny,resourcecontrol=deny"}
	args = append(args, audioArgs(audio)...)
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdin = strings.NewReader("{\"execute\":\"qmp_capabilities\"}\n{\"execute\":\"query-commands\"}\n{\"execute\":\"quit\"}\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("machine probe: %w: %s", err, strings.TrimSpace(string(out)))
	}
	commands := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		var reply struct {
			Commands []struct{ Name string } `json:"return"`
		}
		if json.Unmarshal([]byte(line), &reply) == nil {
			for _, c := range reply.Commands {
				commands[c.Name] = true
			}
		}
	}
	for _, name := range []string{"stop", "cont", "balloon", "query-status", "migrate", "migrate-incoming", "migrate_cancel", "query-migrate"} {
		if !commands[name] {
			return fmt.Errorf("QMP command %s unavailable", name)
		}
	}
	return nil
}

func probeVirtiofsd(ctx context.Context, binary string) error {
	dir, err := os.MkdirTemp("", "clawk-virtiofs-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	share := filepath.Join(dir, "share")
	if err = os.Mkdir(share, 0o700); err != nil {
		return err
	}
	sock, log := filepath.Join(dir, "fs.sock"), filepath.Join(dir, "fs.log")
	p, err := launch(exec.Command(binary, virtiofsArgs(share, sock, true)...), log)
	if err != nil {
		return err
	}
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		p.stop(stop)
	}()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err = waitPath(ctx, sock, p); err != nil {
		out, _ := os.ReadFile(log)
		return fmt.Errorf("sandbox probe: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func virtiofsArgs(share, sock string, readOnly bool) []string {
	args := []string{"--shared-dir", share, "--socket-path", sock, "--cache", "auto", "--sandbox", "namespace", "--seccomp", "kill", "--migration-mode", "find-paths", "--migration-on-error", "abort", "--migration-verify-handles"}
	if readOnly {
		args = append(args, "--readonly")
	}
	return args
}
