//go:build linux && amd64

package kernel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQEMUOverride(t *testing.T) {
	body := []byte("feature kernel")
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }))
	defer s.Close()
	cache := t.TempDir()
	p, err := FetchQEMU(context.Background(), cache, "amd64", s.URL)
	require.NoError(t, err)
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	require.Equal(t, body, b)
	local := filepath.Join(t.TempDir(), "override")
	require.NoError(t, os.WriteFile(local, []byte("custom"), 0600))
	p, err = FetchQEMU(context.Background(), cache, "amd64", local)
	require.NoError(t, err)
	require.Equal(t, local, p)

}

func TestQEMURejectsUnsupportedArchitecture(t *testing.T) {
	remote := "https://invalid.invalid/kernel"
	local := filepath.Join(t.TempDir(), "override")
	require.NoError(t, os.WriteFile(local, []byte("custom"), 0600))
	cache := filepath.Join(t.TempDir(), "cache")
	for _, arch := range []string{"arm64", "unsupported"} {
		for _, override := range []string{"", local, remote} {
			_, err := FetchQEMU(context.Background(), cache, arch, override)
			require.ErrorContains(t, err, "unsupported QEMU architecture")
		}
	}
	require.NoDirExists(t, cache)
}
