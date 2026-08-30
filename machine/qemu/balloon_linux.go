//go:build linux && amd64

package qemu

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/clawkwork/clawk/machine"
)

func (v *vm) SetMemoryTarget(ctx context.Context, bytes uint64) error {
	v.mu.Lock()
	q, state := v.qmp, v.state
	v.mu.Unlock()
	if q == nil || state != machine.StateRunning {
		return machine.ErrInvalidState
	}
	if bytes < 64<<20 || bytes > max(v.spec.MemoryMiB, v.spec.MemoryMaxMiB)<<20 {
		return fmt.Errorf("invalid balloon target %d", bytes)
	}
	_, err := q.command(ctx, "balloon", map[string]any{"value": bytes})
	return err
}

// MemoryTarget reports actual guest-visible memory, including automatic OOM
// deflation. The controller must not assume its last request remains actual.
func (v *vm) MemoryTarget(ctx context.Context) (uint64, error) {
	v.mu.Lock()
	q, state := v.qmp, v.state
	v.mu.Unlock()
	if q == nil || state != machine.StateRunning {
		return 0, machine.ErrInvalidState
	}
	raw, err := q.command(ctx, "query-balloon", nil)
	if err != nil {
		return 0, err
	}
	var report struct {
		Actual uint64 `json:"actual"`
	}
	if err = json.Unmarshal(raw, &report); err != nil {
		return 0, err
	}
	return report.Actual, nil
}
