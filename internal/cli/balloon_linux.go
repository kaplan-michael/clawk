//go:build linux && amd64

package cli

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/clawkwork/clawk/machine"
	"github.com/clawkwork/clawk/machine/balloon"
	"github.com/clawkwork/clawk/machine/guest"
)

func startQEMUBalloon(ctx context.Context, m machine.Machine, spec machine.Spec, logger *log.Logger) {
	actuator, ok := m.(interface {
		SetMemoryTarget(context.Context, uint64) error
		MemoryTarget(context.Context) (uint64, error)
	})
	if !ok {
		return
	}
	baseline := spec.MemoryMiB << 20
	ceiling := max(spec.MemoryMaxMiB, spec.MemoryMiB) << 20
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		current := ceiling
		var policy balloon.Policy
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			poll, cancel := context.WithTimeout(ctx, 4*time.Second)
			s, err := guest.FetchStats(poll, m)
			if err == nil {
				current, err = actuator.MemoryTarget(poll)
			}
			cancel()
			if err != nil {
				continue
			}
			target := policy.Target(current, baseline, ceiling, linuxMemoryPressure(), s)
			if target == current {
				continue
			}
			command, cancel := context.WithTimeout(ctx, 3*time.Second)
			err = actuator.SetMemoryTarget(command, target)
			cancel()
			if err != nil {
				logger.Printf("balloon: %v", err)
				continue
			}
			logger.Printf("balloon: target %d -> %d MiB", current>>20, target>>20)
			current = target
		}
	}()
}
func linuxMemoryPressure() int {
	b, err := os.ReadFile("/proc/pressure/memory")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "some" {
			continue
		}
		n, _ := strconv.ParseFloat(strings.TrimPrefix(fields[1], "avg10="), 64)
		if n >= 20 {
			return 2
		}
		if n >= 5 {
			return 1
		}
	}
	return 0
}
