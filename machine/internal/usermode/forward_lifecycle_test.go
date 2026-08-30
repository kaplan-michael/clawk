//go:build darwin || linux

package usermode

import (
	"net"
	"path/filepath"
	"testing"

	"github.com/clawkwork/clawk/machine"
	"github.com/stretchr/testify/require"
)

func TestForwardPortsReleasedOnCloseAndFailedStart(t *testing.T) {
	reserve := func() net.Listener {
		t.Helper()
		l, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		return l
	}
	first, occupied := reserve(), reserve()
	defer occupied.Close()
	addr := first.Addr().String()
	port := first.Addr().(*net.TCPAddr).Port
	require.NoError(t, first.Close())
	cfg := Config{SockPath: filepath.Join(t.TempDir(), "net.sock"), Forwards: []machine.PortForward{{HostPort: uint16(port), GuestPort: 8080}}}
	s, err := Start(cfg)
	require.NoError(t, err)
	defer s.VMSocket.Close()
	require.NoError(t, s.Close())
	require.NoError(t, s.Close(), "close must be idempotent")
	l, err := net.Listen("tcp", addr)
	require.NoError(t, err)
	require.NoError(t, l.Close())
	cfg.Forwards = append(cfg.Forwards, machine.PortForward{HostPort: uint16(occupied.Addr().(*net.TCPAddr).Port), GuestPort: 8081})
	_, err = Start(cfg)
	require.Error(t, err)
	l, err = net.Listen("tcp", addr)
	require.NoError(t, err, "failed startup must release earlier forwards")
	require.NoError(t, l.Close())
}
