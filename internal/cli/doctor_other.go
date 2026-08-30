//go:build !linux

package cli

import "github.com/clawkwork/clawk/internal/config"

// platformHostChecks is a no-op on non-Linux hosts. macOS links
// Virtualization.framework directly into the binary (see machine/vz), so
// there is no external hypervisor binary or device node to probe — the
// entitlements check happens at codesign time, not runtime.
func platformHostChecks() []doctorCheck { return nil }

func platformHostChecksFor(p config.Provider) []doctorCheck {
	if p == config.ProviderQEMU {
		return []doctorCheck{fail("host: provider", "QEMU requires Linux amd64 (x86-64)", "use a Linux amd64 (x86-64) host")}
	}
	return platformHostChecks()
}

func providerRecordChecks(*config.Sandbox, config.Provider) []doctorCheck { return nil }
