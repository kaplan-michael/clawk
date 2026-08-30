//go:build linux && amd64

package qemu

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// child has one waiter. Exit observation is repeatable and never consumes
// the notification another lifecycle operation is waiting for.
type child struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error // immutable once done is closed
	once sync.Once
}

func launch(cmd *exec.Cmd, logPath string) (*child, error) {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		f.Close()
		return nil, err
	}
	p := &child{cmd: cmd, done: make(chan struct{})}
	go func() { p.err = cmd.Wait(); f.Close(); close(p.done) }()
	return p, nil
}

func (p *child) exited() bool {
	if p == nil {
		return true
	}
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}
func (p *child) kill() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		if !p.exited() {
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		}
	})
}
func (p *child) stop(ctx context.Context) {
	if p == nil {
		return
	}
	p.kill()
	select {
	case <-p.done:
	case <-ctx.Done():
	}
}
func waitPath(ctx context.Context, path string, p *child) error {
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.done:
			if p.err != nil {
				return p.err
			}
			return os.ErrClosed
		case <-t.C:
		}
	}
}
