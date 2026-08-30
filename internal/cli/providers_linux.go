//go:build linux

package cli

import (
	"fmt"
	"runtime"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
)

// newProvider maps a Provider identifier to a constructed provider
// for this host's OS. Identifiers that are valid on the type but not on
// this OS are rejected with a clear "platform" error rather than a
// runtime-only "unsupported" surface from a stub method.
func newProvider(p config.Provider) (sandbox.Provider, error) {
	if runtime.GOARCH != "amd64" {
		return nil, fmt.Errorf("clawk on Linux requires Linux amd64 (x86-64); linux/%s is unsupported", runtime.GOARCH)
	}
	switch p {
	case config.ProviderFirecracker:
		return sandbox.NewFirecrackerProvider(store), nil
	case config.ProviderQEMU:
		return newQEMUProvider()
	case config.ProviderVZ:
		return nil, fmt.Errorf("provider %q runs on macOS; this is a Linux build", p)
	default:
		return nil, fmt.Errorf("unknown provider %q (valid on Linux: firecracker, qemu)", p)
	}
}

// defaultProvider is the provider used for new sandboxes when neither
// --provider nor a clawk.mod selects one. vz cannot run on Linux, so the
// only sensible default here is firecracker.
func defaultProvider() config.Provider { return config.ProviderFirecracker }
