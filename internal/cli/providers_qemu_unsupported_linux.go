//go:build linux && !amd64

package cli

import (
	"fmt"
	"runtime"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
)

// Unsupported builds only diagnose saved/configured provider selections;
// they contain neither a QEMU provider nor its daemon or machine backend.
func newQEMUProvider() (sandbox.Provider, error) {
	return nil, fmt.Errorf("QEMU microvm requires Linux amd64 (x86-64); linux/%s is unsupported", runtime.GOARCH)
}

func qemuHostChecks() []doctorCheck {
	_, err := newQEMUProvider()
	return []doctorCheck{fail("host: provider", err.Error(), "use a Linux amd64 (x86-64) host")}
}

func qemuRecordChecks(*config.Sandbox) []doctorCheck { return nil }
