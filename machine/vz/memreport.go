package vz

import (
	"context"

	"github.com/clawkwork/clawk/machine"
	"github.com/clawkwork/clawk/machine/guest"
)

type memReport = guest.Stats

const agentMemPort uint32 = 1027

func decodeMemReport(b []byte) (memReport, error) { return guest.DecodeStats(b) }

// GuestStats is the exported view of the guest agent's snapshot, for
// callers outside the balloon controller (the sandbox daemon's idle
// watchdog). Field semantics match memReport.
type GuestStats struct {
	TotalKiB        uint64
	AvailableKiB    uint64
	PSIMemSomeCenti uint64
	Load1Centi      uint64
	NetIOBytes      uint64
	SwapTotalKiB    uint64
	SwapFreeKiB     uint64

	// HasActivity is false when the guest runs a legacy agent without the
	// activity fields; Load1Centi and NetIOBytes are then meaningless.
	HasActivity bool
}

// FetchGuestStats dials the guest agent's snapshot port on m and returns
// one decoded report. It works on any machine whose backend implements
// VSock and whose guest runs the clawk agent — the protocol is defined by
// internal/agentembed, but machine/guest owns the host-side decoding, so
// the fetch lives here rather than growing a third copy of the wire
// format. Callers bound the dial via ctx.
func FetchGuestStats(ctx context.Context, m machine.Machine) (GuestStats, error) {
	r, err := guest.FetchStats(ctx, m)
	if err != nil {
		return GuestStats{}, err
	}
	return GuestStats{
		TotalKiB:        r.TotalKiB,
		AvailableKiB:    r.AvailableKiB,
		PSIMemSomeCenti: r.PSIMemSomeCenti,
		Load1Centi:      r.Load1Centi,
		NetIOBytes:      r.NetIOBytes,
		SwapTotalKiB:    r.SwapTotalKiB,
		SwapFreeKiB:     r.SwapFreeKiB,
		HasActivity:     r.HasActivity,
	}, nil
}
