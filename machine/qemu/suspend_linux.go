//go:build linux && amd64

package qemu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/clawkwork/clawk/machine"
	"golang.org/x/sys/unix"
)

const suspendFile = "qemu.state"

type suspendRecord struct {
	Version                                                         int
	Arch, Host, QEMU, Virtiofsd, Fingerprint, Shares, Kernel, Disks string
	CID                                                             uint32
}

// treeDigest records host truth, not a copy to restore. Never follow symlinks
// outside an export, open special files, or accept a file changing during read.
func treeDigest(ctx context.Context, shares []machine.Share) (string, error) {
	h := sha256.New()
	for _, sh := range shares {
		fmt.Fprintf(h, "%s\x00%s\x00%t\n", sh.Tag, sh.HostPath, sh.ReadOnly)
		// An export itself may be a symlink (common for host configuration
		// directories). Fingerprint the exported tree, not just that link.
		root, err := filepath.EvalSymlinks(sh.HostPath)
		if err != nil {
			return "", err
		}
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			st, err := os.Lstat(path)
			if err != nil {
				return err
			}
			stat := st.Sys().(*syscall.Stat_t)
			fmt.Fprintf(h, "%s\x00%d %d %d %d %d %d %d %v\n", path, st.Mode(), st.Size(), st.ModTime().UnixNano(), stat.Dev, stat.Ino, stat.Uid, stat.Gid, stat.Ctim)
			switch {
			case st.Mode().IsRegular():
				sum, err := digestFile(ctx, path)
				if err != nil {
					return err
				}
				fmt.Fprintln(h, sum)
			case st.Mode()&os.ModeSymlink != 0:
				dest, err := os.Readlink(path)
				if err != nil {
					return err
				}
				fmt.Fprintln(h, dest)
			case st.IsDir():
			default:
				return fmt.Errorf("share contains non-restorable special file %s", path)
			}
			after, err := os.Lstat(path)
			if err != nil {
				return err
			}
			if !os.SameFile(st, after) || st.Size() != after.Size() || st.ModTime() != after.ModTime() {
				return fmt.Errorf("share changed while checking %s", path)
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func digestFile(ctx context.Context, path string) (string, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file: %s", path)
	}
	h := sha256.New()
	buf := make([]byte, 128<<10)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, e := f.Read(buf)
		if n > 0 {
			_, _ = h.Write(buf[:n])
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return "", e
		}
	}
	after, err := f.Stat()
	if err != nil {
		return "", err
	}
	if st.Size() != after.Size() || st.ModTime() != after.ModTime() {
		return "", fmt.Errorf("file changed while checking %s", path)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (v *vm) record(ctx context.Context) (suspendRecord, error) {
	r := suspendRecord{Version: 1, Arch: GuestArch(), CID: v.spec.VSockCID}
	var err error
	for _, path := range []string{"/etc/machine-id", "/proc/sys/kernel/osrelease"} {
		b, e := os.ReadFile(path)
		if e != nil {
			return r, e
		}
		r.Host += string(b)
	}
	cpu, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return r, err
	}
	for _, line := range strings.Split(string(cpu), "\n") {
		key, _, _ := strings.Cut(line, ":")
		switch strings.TrimSpace(key) {
		case "vendor_id", "cpu family", "model", "model name", "stepping", "microcode", "flags":
			r.Host += line + "\n"
		}
	}
	r.QEMU, err = probeOutput(ctx, v.binary, "--version")
	if err != nil {
		return r, err
	}
	qsum, err := digestResolvedFile(ctx, v.binary)
	if err != nil {
		return r, err
	}
	r.QEMU += qsum
	if len(v.spec.Shares) > 0 {
		binary, e := FindVirtiofsd()
		if e != nil {
			return r, e
		}
		r.Virtiofsd, err = probeOutput(ctx, binary, "--version")
		if err != nil {
			return r, err
		}
		sum, e := digestResolvedFile(ctx, binary)
		if e != nil {
			return r, e
		}
		r.Virtiofsd += sum
	}
	// Encode full device configuration, not just the count-based common meta.
	spec := v.spec
	spec.Net = append([]machine.Net(nil), spec.Net...)
	for i, n := range spec.Net {
		if um, ok := n.(machine.UserMode); ok {
			um.Filter = nil // policy is reapplied by the host on every start
			spec.Net[i] = um
		}
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return r, err
	}
	hash := sha256.Sum256(append(b, []byte(cpuModel(spec.NestedVirt)+strings.Join(audioArgs(v.audio), "\x00")+v.authToken)...))
	r.Fingerprint = hex.EncodeToString(hash[:])
	r.Shares, err = treeDigest(ctx, v.spec.Shares)
	if err != nil {
		return r, err
	}
	dk := v.spec.Boot.(machine.DirectKernel)
	r.Kernel, err = digestResolvedFile(ctx, dk.Vmlinux)
	if err != nil {
		return r, err
	}
	if dk.Initrd != "" {
		s, e := digestResolvedFile(ctx, dk.Initrd)
		if e != nil {
			return r, e
		}
		r.Kernel += s
	}
	// Disk identity/size/mtime, plus config contents. Writable disk state is
	// exclusively owned by QEMU and must not be changed while suspended.
	disks := append([]machine.Disk{{Path: v.spec.RootFS.(machine.RawDisk).Path}}, v.spec.Disks...)
	for _, d := range disks {
		st, e := os.Stat(d.Path)
		if e != nil {
			return r, e
		}
		x := st.Sys().(*syscall.Stat_t)
		r.Disks += fmt.Sprintf("%s:%d:%d:%d:%d:%v;", d.Path, x.Dev, x.Ino, st.Size(), st.ModTime().UnixNano(), x.Ctim)
		if d.ReadOnly {
			s, e := digestFile(ctx, d.Path)
			if e != nil {
				return r, e
			}
			r.Disks += s
		}
	}
	return r, nil
}

func digestResolvedFile(ctx context.Context, path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return digestFile(ctx, resolved)
}

// Migration uses a private Unix stream owned by the daemon. No shell or helper
// command is launched by QEMU (its seccomp sandbox explicitly forbids that).
func (v *vm) transfer(ctx context.Context, path string, incoming bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sock := filepath.Join(v.stateDir, "migration.sock")
	_ = os.Remove(sock)
	defer os.Remove(sock)
	var listener net.Listener
	var err error
	if !incoming {
		listener, err = net.Listen("unix", sock)
		if err != nil {
			return err
		}
		defer listener.Close()
		_ = os.Chmod(sock, 0o600)
		stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
		defer stop()
	}
	v.mu.Lock()
	q := v.qmp
	v.mu.Unlock()
	if q == nil {
		return machine.ErrInvalidState
	}
	copied := make(chan error, 1)
	go func() {
		var c net.Conn
		var e error
		if incoming {
			for {
				c, e = (&net.Dialer{}).DialContext(ctx, "unix", sock)
				if e == nil {
					break
				}
				select {
				case <-ctx.Done():
					copied <- ctx.Err()
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
		} else {
			c, e = listener.Accept()
			if e != nil {
				copied <- e
				return
			}
		}
		defer c.Close()
		stop := context.AfterFunc(ctx, func() { _ = c.Close() })
		defer stop()
		flags := os.O_CREATE | os.O_WRONLY | os.O_EXCL
		if incoming {
			flags = os.O_RDONLY
		}
		f, e := os.OpenFile(path, flags, 0o600)
		if e != nil {
			copied <- e
			return
		}
		defer f.Close()
		if incoming {
			_, e = io.Copy(c, f)
		} else {
			_, e = io.Copy(f, c)
			if e == nil {
				e = f.Sync()
			}
		}
		copied <- e
	}()
	name := "migrate"
	if incoming {
		name = "migrate-incoming"
	}
	if _, err = q.command(ctx, name, map[string]any{"uri": "unix:" + sock}); err != nil {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	copyDone := false
	for {
		raw, e := q.command(ctx, "query-migrate", nil)
		if e != nil {
			return e
		}
		var status struct {
			Status string `json:"status"`
			Error  string `json:"error-desc"`
		}
		if e = json.Unmarshal(raw, &status); e != nil {
			return e
		}
		switch status.Status {
		case "completed":
			if copyDone {
				return nil
			}
			select {
			case e := <-copied:
				return e
			case <-ctx.Done():
				return ctx.Err()
			}
		case "failed", "cancelled":
			select {
			case err := <-copied:
				if err != nil {
					return fmt.Errorf("migration stream: %w", err)
				}
			default:
			}
			return fmt.Errorf("migration %s: %s", status.Status, status.Error)
		}
		select {
		case err := <-copied:
			if err != nil {
				return fmt.Errorf("migration stream: %w", err)
			}
			copyDone = true
			copied = nil
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (v *vm) Suspend(ctx context.Context, dir string) error { return v.save(ctx, dir, true) }

func (v *vm) Snapshot(ctx context.Context, dir string) error { return v.save(ctx, dir, false) }

func (v *vm) save(ctx context.Context, dir string, stop bool) (retErr error) {
	ctx, cancel := context.WithTimeout(ctx, DefaultStartupBudgets().Migration)
	defer cancel()
	if err := v.opMu.LockContext(ctx); err != nil {
		return err
	}
	defer v.opMu.Unlock()
	v.mu.Lock()
	state, q := v.state, v.qmp
	v.mu.Unlock()
	if q == nil || (state != machine.StateRunning && state != machine.StatePaused) {
		return machine.ErrInvalidState
	}
	if !stop && state != machine.StateRunning {
		return machine.ErrInvalidState
	}
	if v.syncGuest != nil && state == machine.StateRunning {
		if err := v.syncGuest(ctx); err != nil {
			return err
		}
	}
	if _, err := q.command(ctx, "stop", nil); err != nil {
		return err
	}
	v.mu.Lock()
	v.state = machine.StatePaused
	v.mu.Unlock()
	defer func() {
		if retErr != nil {
			recovery, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = q.command(recovery, "migrate_cancel", nil)
			if state == machine.StateRunning {
				if _, e := q.command(recovery, "cont", nil); e == nil {
					v.mu.Lock()
					v.state = machine.StateRunning
					v.mu.Unlock()
				}
			}
			_ = os.Remove(filepath.Join(dir, suspendFile))
			_ = os.Remove(filepath.Join(dir, "qemu.json"))
		}
	}()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	before, err := v.record(ctx)
	if err != nil {
		return fmt.Errorf("suspend consistency: %w", err)
	}
	path := filepath.Join(dir, suspendFile+".partial")
	_ = os.Remove(path)
	defer os.Remove(path)
	if err = v.transfer(ctx, path, false); err != nil {
		return err
	}
	after, err := v.record(ctx)
	if err != nil {
		return err
	}
	if before != after {
		return errors.New("host files changed during suspend; VM kept running")
	}
	b, err := json.Marshal(after)
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "qemu.json"), b, 0o600); err != nil {
		return err
	}
	if err = os.Rename(path, filepath.Join(dir, suspendFile)); err != nil {
		return err
	}
	if stop {
		v.cleanup() // never resume a suspended source past its save point
	} else if _, err := q.command(ctx, "cont", nil); err != nil {
		return err
	}
	v.mu.Lock()
	if stop {
		v.state = machine.StateStopped
	} else {
		v.state = machine.StateRunning
	}
	v.mu.Unlock()
	return nil
}
func (v *vm) Restore(ctx context.Context, dir string) (retErr error) {
	ctx, cancel := context.WithTimeout(ctx, DefaultStartupBudgets().Migration)
	defer cancel()
	if err := v.opMu.LockContext(ctx); err != nil {
		return err
	}
	defer v.opMu.Unlock()
	v.mu.Lock()
	state := v.state
	v.mu.Unlock()
	if state != machine.StateCreated && state != machine.StateStopped {
		return machine.ErrInvalidState
	}
	b, err := os.ReadFile(filepath.Join(dir, "qemu.json"))
	if err != nil {
		return err
	}
	var saved suspendRecord
	if err = json.Unmarshal(b, &saved); err != nil {
		return err
	}
	now, err := v.record(ctx)
	if err != nil {
		return err
	}
	if saved != now {
		return errors.New("saved state no longer matches host files, disks, kernel or QEMU configuration; host files remain authoritative")
	}
	v.incoming = true
	defer func() {
		v.incoming = false
		if retErr != nil {
			v.cleanup()
			v.mu.Lock()
			v.state = machine.StateStopped
			v.mu.Unlock()
		}
	}()
	if err = v.start(ctx); err != nil {
		return err
	}
	if err = v.transfer(ctx, filepath.Join(dir, suspendFile), true); err != nil {
		return err
	}
	after, err := v.record(ctx)
	if err != nil {
		return err
	}
	if after != saved {
		return errors.New("host files changed during restore")
	}
	v.mu.Lock()
	q := v.qmp
	v.mu.Unlock()
	if _, err = q.command(ctx, "cont", nil); err != nil {
		return err
	}
	v.mu.Lock()
	v.state = machine.StateRunning
	v.mu.Unlock()
	return nil
}

var _ machine.Suspendable = (*vm)(nil)
var _ machine.Snapshottable = (*vm)(nil)
