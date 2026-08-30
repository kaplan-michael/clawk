//go:build !linux

package cli

import (
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/machine"
	"github.com/stretchr/testify/require"
)

func TestQEMUUnavailableOnUnsupportedHost(t *testing.T) {
	p, err := newProvider(config.ProviderQEMU)
	require.ErrorContains(t, err, "requires Linux amd64")
	require.Nil(t, p)
	_, err = machine.Get("qemu")
	require.Error(t, err)
	for _, cmd := range rootCmd.Commands() {
		require.NotEqual(t, "__qemud", cmd.Name(), "unsupported builds must exclude the QEMU daemon")
	}
}

func TestDoctorRejectsUnsupportedQEMUWithoutPackageHints(t *testing.T) {
	checks := platformHostChecksFor(config.ProviderQEMU)
	require.Len(t, checks, 1)
	require.Equal(t, statusFail, checks[0].Status)
	require.Contains(t, checks[0].Detail, "requires Linux amd64")
	require.NotContains(t, checks[0].FixHint, "install")
	require.Empty(t, providerRecordChecks(nil, config.ProviderQEMU))
}
