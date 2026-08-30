//go:build linux && amd64

package cli

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Use a private null sink's monitor through PulseAudio (including PipeWire's
// Pulse server). This neither records a microphone nor changes desktop defaults.
func setupQEMUTestAudio(t *testing.T) {
	t.Helper()
	socket := filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "pulse/native")
	_, err := os.Stat(socket)
	if err != nil {
		// A private PulseAudio server also makes this work on headless CI.
		// No real sound devices or desktop session are necessary.
		dir := t.TempDir()
		socket = filepath.Join(dir, "native")
		server := exec.Command("pulseaudio", "--daemonize=no", "--use-pid-file=no", "--exit-idle-time=-1", "-n", "--load=module-native-protocol-unix socket="+socket)
		server.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+dir, "PULSE_RUNTIME_PATH="+dir)
		server.Stderr = os.Stderr
		require.NoError(t, server.Start(), "headless PCM test needs pulseaudio, pactl and paplay")
		t.Cleanup(func() { _ = server.Process.Kill(); _ = server.Wait() })
		require.Eventually(t, func() bool { _, err := os.Stat(socket); return err == nil }, 5*time.Second, 20*time.Millisecond)
	}
	t.Setenv("PULSE_SERVER", "unix:"+socket)
	sink := fmt.Sprintf("clawk_test_%d", os.Getpid())
	out, err := exec.Command("pactl", "load-module", "module-null-sink", "sink_name="+sink).CombinedOutput()
	require.NoError(t, err, string(out))
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("pactl", "unload-module", id).Run() })
	// Select the Pulse backend for this subprocess without changing the host
	// session or adding test switches to the provider's production API.
	runtime := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(runtime, "pulse"), 0o700))
	require.NoError(t, os.Symlink(socket, filepath.Join(runtime, "pulse/native")))
	t.Setenv("XDG_RUNTIME_DIR", runtime)
	t.Setenv("PULSE_SOURCE", sink+".monitor")
	cmd := exec.Command("paplay", "--raw", "--rate=48000", "--channels=2", "--format=s16le", "--device="+sink)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer stdin.Close()
		buf := make([]byte, 4800*4) // exactly 44 periods: loop without a phase discontinuity
		for i := 0; i < 4800; i++ {
			v := uint16(int16(8000 * math.Sin(2*math.Pi*440*float64(i)/48000)))
			binary.LittleEndian.PutUint16(buf[4*i:], v)
			binary.LittleEndian.PutUint16(buf[4*i+2:], v)
		}
		for {
			if _, err := stdin.Write(buf); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = stdin.Close(); <-done; _ = cmd.Wait() })
}
