//go:build linux && amd64

package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/stretchr/testify/require"
)

func TestDoctorUsesConfiguredKernelBeforeCreation(t *testing.T) {
	setupTest(t)
	t.Chdir(t.TempDir())
	withGlobalMod(t, "sandbox (\n vm (\n provider qemu\n kernel /configured/kernel\n )\n)\n")
	require.Empty(t, providerRecordChecks(nil, config.ProviderQEMU), "a configured kernel must not be diagnosed as the default fallback")
}

func TestDoctorChecksConfiguredNestedPrerequisites(t *testing.T) {
	setupTest(t)
	t.Chdir(t.TempDir())
	withGlobalMod(t, "sandbox (\n vm (\n provider qemu\n nested\n )\n)\n")
	checks := providerRecordChecks(nil, config.ProviderQEMU)
	found := false
	for _, check := range checks {
		if check.Name == "guest: nested virtualization" {
			found = true
			require.Equal(t, statusFail, check.Status)
		}
	}
	require.True(t, found, "missing requested kernel features must fail before sandbox creation")
}

func TestDoctorDefaultQEMUKernelSupported(t *testing.T) {
	setupTest(t)
	checks := providerRecordChecks(&config.Sandbox{Name: "default-kernel"}, config.ProviderQEMU)
	require.NotEmpty(t, checks)
	for _, check := range checks {
		require.Equal(t, statusOK, check.Status, check.Detail)
	}
}

func TestQEMUSpecPreparationCancellation(t *testing.T) {
	setupTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { cancel(); <-r.Context().Done() }))
	defer server.Close()
	sb := &config.Sandbox{Name: "cancel", Kernel: server.URL}
	_, err := buildQEMUSpec(ctx, sb, store.VMDir(sb.Key()), nil, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.NoFileExists(t, filepath.Join(store.VMDir(sb.Key()), "guestcfg.img"))
}
