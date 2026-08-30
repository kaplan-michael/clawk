//go:build linux && amd64

package qemu

import "time"

// StartupBudgets bounds each startup phase. The provider's outer deadline
// includes every phase and cleanup; a valid migration must not be killed by
// a shorter agent wait in the parent process.
type StartupBudgets struct {
	Kernel, Preparation, Migration, GuestReady, Cleanup time.Duration
}

func DefaultStartupBudgets() StartupBudgets {
	return StartupBudgets{Kernel: 10 * time.Minute, Preparation: 2 * time.Minute, Migration: 10 * time.Minute, GuestReady: 2 * time.Minute, Cleanup: 15 * time.Second}
}

func (b StartupBudgets) Total(restore bool) time.Duration {
	total := b.Kernel + b.Preparation + b.GuestReady + b.Cleanup
	if restore {
		total += b.Migration
	}
	return total
}
