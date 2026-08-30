//go:build linux && amd64

package qemu

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/clawkwork/clawk/machine"
	"github.com/stretchr/testify/require"
)

func baseSpec() machine.Spec {
	return machine.Spec{ID: "test", VCPU: 1, MemoryMiB: 128, Boot: machine.DirectKernel{Vmlinux: "kernel"}, RootFS: machine.RawDisk{Path: "root"}, VSockCID: 3}
}

func TestValidateKernel(t *testing.T) {
	p := filepath.Join(t.TempDir(), "vmlinuz")
	b := make([]byte, 0x206)
	copy(b[0x202:], "HdrS")
	require.NoError(t, os.WriteFile(p, b, 0o644))
	require.NoError(t, ValidateKernel(p))
	require.NoError(t, os.WriteFile(p, make([]byte, 0x206), 0o644))
	require.ErrorContains(t, ValidateKernel(p), "not an x86 bzImage")
}

func TestCheckSpecRejectsMultipleNetworksAndDuplicateTags(t *testing.T) {
	s := baseSpec()
	s.Net = []machine.Net{machine.UserMode{}, machine.TAP{Device: "tap0"}}
	require.ErrorIs(t, checkSpec(s), machine.ErrUnsupportedSpec)
	s = baseSpec()
	s.Shares = []machine.Share{{Tag: "same", HostPath: "/a"}, {Tag: "same", HostPath: "/b"}}
	require.ErrorIs(t, checkSpec(s), machine.ErrUnsupportedSpec)
}

func TestBuildArgsRequiresKVMAndEscapesCommas(t *testing.T) {
	v := &vm{spec: baseSpec(), stateDir: "/state,dir"}
	v.spec.RootFS = machine.RawDisk{Path: "/root,disk"}
	args, err := v.buildArgs()
	require.NoError(t, err)
	require.Equal(t, "-machine", args[0])
	require.Regexp(t, `^microvm,`, args[1])
	require.Contains(t, args[1], "accel=kvm")
	require.Contains(t, args[3], "host,")
	require.Contains(t, args[1], "kernel=kernel")
	require.Contains(t, args, "if=none,id=disk0,file=/root,,disk,format=raw,readonly=off")
	require.Contains(t, args, "on,elevateprivileges=deny,spawn=deny,obsolete=deny,resourcecontrol=deny")
	require.Contains(t, args, "virtio-balloon-device,deflate-on-oom=on")
}

func TestQEMUArgsNeverOfferTCGFallback(t *testing.T) {
	v := &vm{spec: baseSpec(), stateDir: "/state"}
	v.spec.RootFS = machine.RawDisk{Path: "/root"}
	args, err := v.buildArgs()
	require.NoError(t, err)
	require.NotContains(t, args, "tcg")
	require.Contains(t, args[1], "accel=kvm")
}

func TestAudioReservesPCISlotWithoutConsumingMMIO(t *testing.T) {
	v := &vm{spec: baseSpec(), stateDir: "/state", audio: "none"}
	v.spec.Shares = make([]machine.Share, 31)
	_, err := v.buildArgs()
	require.ErrorContains(t, err, "30 available PCI slots")
	v.spec.Shares = v.spec.Shares[:30]
	// Root, four extra disks, RNG, balloon and vsock fill all eight MMIO
	// transports. HDA and the filesystem devices must remain on PCI/MSI.
	v.spec.Disks = make([]machine.Disk, 4)
	args, err := v.buildArgs()
	require.NoError(t, err)
	require.Contains(t, args, "intel-hda,id=hda0,msi=on")
	require.Contains(t, args, "hda-micro,bus=hda0.0,audiodev=audio0")
	require.Contains(t, args, "-only-migratable")
}

func TestCheckSpecAcceptsMicroVMVerticalSlice(t *testing.T) {
	s := baseSpec()
	s.Net = []machine.Net{machine.UserMode{}}
	s.Shares = []machine.Share{{Tag: "workspace", HostPath: "/tmp"}}
	require.NoError(t, checkSpec(s))
}

func TestCheckSpecRejectsInvalidShareTag(t *testing.T) {
	s := baseSpec()
	s.Shares = []machine.Share{{Tag: "bad,tag", HostPath: "/tmp"}}
	require.ErrorIs(t, checkSpec(s), machine.ErrUnsupportedSpec)
}

func TestCapabilities(t *testing.T) {
	c := backend{}.Capabilities()
	require.True(t, c.DirectKernel)
	require.True(t, c.VSock)
	require.True(t, c.VirtioFS)
	require.True(t, c.UserModeNet)
	require.False(t, c.UnixgramNet)
	require.True(t, c.Snapshot)
}

func TestPersistentCIDIsStableDistinctAndPrivate(t *testing.T) {
	a, b := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	require.NoError(t, os.MkdirAll(a, 0o700))
	require.NoError(t, os.MkdirAll(b, 0o700))
	c1, err := persistentCID(a, "alpha")
	require.NoError(t, err)
	c2, err := persistentCID(a, "changed-name")
	require.NoError(t, err)
	c3, err := persistentCID(b, "beta")
	require.NoError(t, err)
	require.Equal(t, c1, c2)
	require.NotEqual(t, c1, c3)
	st, err := os.Stat(filepath.Join(a, "vsock.cid"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), st.Mode().Perm())
}
