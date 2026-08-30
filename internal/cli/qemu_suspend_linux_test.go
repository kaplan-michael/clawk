//go:build linux && amd64

package cli

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/clawkwork/clawk/internal/agentembed"
	"github.com/clawkwork/clawk/machine"
	"github.com/stretchr/testify/require"
)

// Guest updates must invalidate memory before disk selection; otherwise a
// successful restore can keep obsolete PID 1 code alive indefinitely.
func TestPrepareQEMUSuspendState(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta *machine.SuspendMeta
		keep bool
	}{
		{"current", &machine.SuspendMeta{GuestSources: agentembed.SourcesHash(), Image: "image:v1"}, true},
		{"old sources", &machine.SuspendMeta{GuestSources: "old-init", Image: "image:v1"}, false},
		{"changed image", &machine.SuspendMeta{GuestSources: agentembed.SourcesHash(), Image: "image:v0"}, false},
		{"missing metadata", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vmDir := t.TempDir()
			dir := suspendStateDir(vmDir)
			require.NoError(t, os.MkdirAll(dir, 0700))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "state.bin"), []byte("memory"), 0600))
			disk := filepath.Join(vmDir, "disk.raw")
			require.NoError(t, os.WriteFile(disk, []byte("guest disk"), 0600))
			if tc.meta != nil {
				require.NoError(t, machine.WriteSuspendMeta(dir, *tc.meta))
			}
			require.NoError(t, prepareQEMUSuspendState(vmDir, "image:v1", log.New(io.Discard, "", 0)))
			require.Equal(t, tc.keep, hasSuspendState(dir))
			contents, err := os.ReadFile(disk)
			require.NoError(t, err)
			require.Equal(t, "guest disk", string(contents))
		})
	}
}
