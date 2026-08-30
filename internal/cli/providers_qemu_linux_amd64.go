//go:build linux && amd64

package cli

import "github.com/clawkwork/clawk/internal/sandbox"

func newQEMUProvider() (sandbox.Provider, error) {
	return sandbox.NewQEMUProvider(store), nil
}
