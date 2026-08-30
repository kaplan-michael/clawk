//go:build linux && amd64

package qemu

import (
	"context"
	"sync"
)

// contextMutex serializes operations without blocking a cancelled caller.
// Its zero value is ready to use.
type contextMutex struct {
	once  sync.Once
	token chan struct{}
}

func (m *contextMutex) init() { m.once.Do(func() { m.token = make(chan struct{}, 1) }) }
func (m *contextMutex) LockContext(ctx context.Context) error {
	m.init()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case m.token <- struct{}{}:
		if err := ctx.Err(); err != nil {
			m.Unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (m *contextMutex) Lock() { _ = m.LockContext(context.Background()) }
func (m *contextMutex) TryLock() bool {
	m.init()
	select {
	case m.token <- struct{}{}:
		return true
	default:
		return false
	}
}
func (m *contextMutex) Unlock() {
	select {
	case <-m.token:
	default:
		panic("unlock of unlocked context mutex")
	}
}
