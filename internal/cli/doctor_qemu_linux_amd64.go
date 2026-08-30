//go:build linux && amd64

package cli

import (
	"context"
	"os"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/clawkwork/clawk/machine"
	"github.com/clawkwork/clawk/machine/qemu"
)

func qemuHostChecks() []doctorCheck {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var results []doctorCheck
	for _, c := range qemu.CheckHost(ctx) {
		status := statusOK
		if c.Err != nil {
			status = statusFail
			if c.Optional {
				status = statusWarn
			}
		}
		detail := c.Detail
		if c.Err != nil {
			detail = c.Err.Error()
		}
		results = append(results, doctorCheck{Name: "host: " + c.Name, Status: status, Detail: detail, FixHint: c.Fix})
	}
	return results
}

func qemuRecordChecks(sb *config.Sandbox) []doctorCheck {
	var checks []doctorCheck
	requested := sb
	if requested == nil {
		requested = &config.Sandbox{}
		cwd, err := os.Getwd()
		if err != nil {
			return []doctorCheck{fail("config: QEMU", err.Error(), "run doctor from an accessible directory")}
		}
		tmpl, _, _, err := loadHereClawkfile(cwd)
		if err != nil {
			return []doctorCheck{fail("config: QEMU", err.Error(), "correct clawk.mod")}
		}
		if tmpl != nil {
			requested.Kernel, requested.NestedVirt = tmpl.Kernel, tmpl.Nested
		}
	}
	missingFeatureKernel := requested.Kernel == ""
	if missingFeatureKernel {
		checks = append(checks, ok("guest: QEMU kernel", "supported default kernel; optional microphone capture and nested KVM require a custom kernel (see docs/qemu.md)"))
	}
	if requested.NestedVirt {
		if missingFeatureKernel {
			checks = append(checks, fail("guest: nested virtualization", "nested virtualization requested without a KVM-enabled guest kernel", "set vm.kernel to a QEMU kernel built with CONFIG_KVM=y"))
		}
		if backend, err := machine.Get(qemu.Name); err == nil && !backend.Capabilities().NestedVirt {
			checks = append(checks, fail("host: requested nested virtualization", "host KVM does not expose the requested nesting capability", "enable nesting in the host hypervisor, or disable vm.nested"))
		}
	}
	if sb != nil {
		if err := sandbox.CheckQEMUGuest(store.VMDir(sb.Key())); err != nil {
			checks = append(checks, fail("guest: QEMU version", err.Error(), "recreate the sandbox after saving guest-only files"))
		}
	}
	return checks
}
