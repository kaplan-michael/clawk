//go:build linux && amd64

package cli

import (
	"path/filepath"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/netfilter"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/clawkwork/clawk/machine"
)

// buildQEMUBaseSpec translates a sandbox config into the shared parts of
// a machine.Spec: resources, networking, host shares, and the serial
// log. Each daemon supplies its boot mode and rootfs.
func buildQEMUBaseSpec(sb *config.Sandbox, vmDir string, allow *netfilter.AllowList) (machine.Spec, error) {
	forwards := make([]machine.PortForward, 0, len(sb.Forwards))
	for _, f := range sb.Forwards {
		forwards = append(forwards, machine.PortForward{
			HostPort: uint16(f.HostPort), GuestPort: uint16(f.GuestPort),
			Proto: machine.ProtoTCP,
		})
	}
	shares, err := sandbox.QEMUShares(sb, store)
	if err != nil {
		return machine.Spec{}, err
	}
	exports := make([]machine.Share, 0, len(shares))
	for _, sh := range shares {
		exports = append(exports, machine.Share{HostPath: sh.HostPath, Tag: sh.Tag, ReadOnly: sh.ReadOnly})
	}
	vcpu, memMiB, memMaxMiB := specResources(sb)
	return machine.Spec{
		ID:           sb.Key(),
		VCPU:         vcpu,
		MemoryMiB:    memMiB,
		MemoryMaxMiB: memMaxMiB,
		NestedVirt:   sb.NestedVirt,
		Net: []machine.Net{
			machine.UserMode{Forwards: forwards, Filter: allow},
		},
		Shares: exports,
		Serial: machine.Serial{LogPath: filepath.Join(vmDir, "console.log")},
	}, nil
}
