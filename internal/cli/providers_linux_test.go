//go:build linux && amd64

package cli

import (
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/stretchr/testify/require"
)

func TestNewProviderQEMU(t *testing.T) {
	require.Equal(t, config.ProviderFirecracker, defaultProvider())
	p, err := newProvider(config.ProviderQEMU)
	require.NoError(t, err)
	require.IsType(t, &sandbox.QEMUProvider{}, p)
	require.Equal(t, sandbox.WorkspaceRoot, sandbox.GuestWorkspace(p))
}
