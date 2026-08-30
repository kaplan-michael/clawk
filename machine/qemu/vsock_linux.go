//go:build linux && amd64

package qemu

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
	"unsafe"

	"github.com/clawkwork/clawk/machine"
	"github.com/mdlayher/socket"
	mdvsock "github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"
)

// Reserve the CID in the kernel and pass the same open vhost fd to QEMU.
// This covers other users and VMMs, which a per-user allocation file cannot.
func (v *vm) reserve(reallocate bool) error {
	f, err := os.OpenFile(v.stateDir+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return fmt.Errorf("qemu machine already owned by another daemon: %w", err)
	}
	v.lock = f
	if err := v.reserveCID(reallocate); err != nil {
		_ = f.Close()
		v.lock = nil
		return err
	}
	return nil
}

func (v *vm) reserveCID(reallocate bool) error {
	fd, err := os.OpenFile("/dev/vhost-vsock", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	for i := 0; i < 1024; i++ {
		cid := uint64(v.spec.VSockCID)
		_, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd.Fd(), 0x4008af60, uintptr(unsafe.Pointer(&cid)))
		if e == 0 {
			v.vhost = fd
			if err = os.WriteFile(filepath.Join(v.stateDir, "vsock.cid"), []byte(strconv.FormatUint(cid, 10)+"\n"), 0o600); err != nil {
				_ = fd.Close()
				v.vhost = nil
				return err
			}
			return nil
		}
		if e != unix.EADDRINUSE || !reallocate {
			fd.Close()
			return fmt.Errorf("reserve vsock CID %d: %w", cid, e)
		}
		v.spec.VSockCID = 3 + (v.spec.VSockCID-3+1)%60000
	}
	fd.Close()
	return errors.New("no free vsock CID after 1024 attempts")
}
func (v *vm) vhostArg() string {
	if v.vhost != nil {
		return ",vhostfd=3"
	}
	return ""
}

// HostVSockPort maps the guest protocol's logical ports to this sandbox's
// host listener range. The guest receives the same mapping in its manifest.
func (v *vm) HostVSockPort(port uint32) uint32 { return v.spec.VSockCID*4096 + port }

type peerListener struct {
	net.Listener
	cid uint32
}

func (l *peerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if a, ok := c.RemoteAddr().(*mdvsock.Addr); ok && a.ContextID == l.cid {
			return c, nil
		}
		_ = c.Close()
	}
}
func (v *vm) VSockListen(ctx context.Context, port uint32) (net.Listener, error) {
	if port >= 4096 || v.spec.VSockCID > 1048574 {
		return nil, fmt.Errorf("invalid vsock port/CID mapping")
	}
	l, err := mdvsock.Listen(v.HostVSockPort(port), nil)
	if err != nil {
		return nil, err
	}
	filtered := &peerListener{Listener: l, cid: v.spec.VSockCID}
	v.mu.Lock()
	v.listeners = append(v.listeners, filtered)
	v.mu.Unlock()
	return filtered, nil
}

// Nonblocking connect gives the caller a real cancellation bound. mdlayher's
// convenience Dial has no context and can block beyond startup/shutdown budgets.
type vsockConn struct {
	*socket.Conn
	local, remote net.Addr
}

func (c *vsockConn) LocalAddr() net.Addr  { return c.local }
func (c *vsockConn) RemoteAddr() net.Addr { return c.remote }
func dialVSock(ctx context.Context, cid, port uint32) (net.Conn, error) {
	for {
		fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
		c, err := socket.New(fd, "vsock")
		if err != nil {
			_ = unix.Close(fd)
			return nil, err
		}
		local, err := c.Connect(ctx, &unix.SockaddrVM{CID: cid, Port: port})
		if err == nil {
			addr := &mdvsock.Addr{ContextID: 2}
			if a, ok := local.(*unix.SockaddrVM); ok {
				addr.ContextID = a.CID
				addr.Port = a.Port
			}
			return &vsockConn{Conn: c, local: addr, remote: &mdvsock.Addr{ContextID: cid, Port: port}}, nil
		}
		_ = c.Close()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

var _ machine.VSockListener = (*vm)(nil)
