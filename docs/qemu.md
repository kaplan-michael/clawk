# QEMU on Linux amd64

QEMU runs only on Linux amd64 (x86-64), using the `microvm` machine with KVM and
the existing default Kata bzImage. Normal operation needs no custom kernel.
Firecracker remains the Linux default. Saved sandboxes retain their provider.
QEMU is currently experimental; see the Linux amd64 checks in
[the validation guide](qemu-validation.md).

## Install and select

Run as an ordinary user with:

- QEMU 10.1 or newer (`qemu-system-x86_64`), with KVM and seccomp support.
- Rust virtiofsd 1.14 or newer, on PATH or in a standard libexec directory.
- Read/write access to `/dev/kvm` and `/dev/vhost-vsock` and working AF_VSOCK.
- Unprivileged user/mount namespaces permitted by host security policy.

On Fedora, install `qemu-system-x86-core` and `virtiofsd`. On Debian/Ubuntu,
the packages are `qemu-system-x86` and `virtiofsd`; check that their versions
meet the minimums. On Arch, use `qemu-system-x86` and `virtiofsd`.

```sh
clawk doctor --provider qemu
```

Doctor probes the QEMU devices, QMP commands, namespace isolation and seccomp
configuration used at boot, and prints package and permission remedies. With no
name, it also checks the sandbox inferred from the current directory; before
creation, that sandbox may be reported as missing. An existing sandbox is
checked using its saved provider.
Networking uses embedded gvproxy; ordinary QEMU operation needs no external
network helper, bridge, TAP setup, or sudo.

Select QEMU for new sandboxes in `~/.config/clawk/clawk.mod`, or in a project's
sandbox block:

```text
sandbox (
    vm (
        provider qemu
    )
)
```

QEMU uses the existing provider selection rules. `clawk create --provider qemu`
selects it explicitly; project/workspace commands read the composed `vm.provider`
setting. Workspace-level provider settings retain their existing precedence over
`--provider`. Existing sandboxes keep their saved provider.

## Supported behavior

The default kernel supports guest commands and interactive terminals, OCI
images with injected PID 1, host UID/GID mapping, live worktrees and read-only
shares, persistent agent state and session history, filtered networking and
policy reload, TCP forwarding in both directions, SSH-agent forwarding, serial
forwarding, ballooning, idle shutdown, time synchronization, pause/resume and
suspend/restore.

QEMU checks required worktree and user-share paths before boot and fails if
persistent agent storage cannot be prepared. Managed worktrees under the store's
worktree directory share one export; recorded paths elsewhere receive individual
mounts. Missing sources are never replaced with empty directories.

Microvm supports up to 31 filesystem exports, or 30 with HDA audio. Software
emulation (TCG) and EFI are outside this provider's scope. The existing toolchain
cache configuration remains disabled.

## Optional custom kernels

Microphone capture and nested KVM require custom kernels. The
default kernel is a supported configuration without these optional features.
Explicit nested virtualization without a kernel override fails with instructions
for building one. Nesting also requires support from the host KVM configuration.

QEMU audio uses PipeWire or PulseAudio and migratable Intel HDA. The guest
kernel needs built-in `CONFIG_SND_HDA_INTEL=y` and `CONFIG_SND_HDA_GENERIC=y`.
`CLAWK_AUDIO_INPUT=0` disables the host audio device at startup. Audio permission
or session failures appear in doctor and daemon logs.

The QEMU fragment includes HDA, KVM, virtio, PCI and PSI support. Build natively
on Linux amd64, using the existing fragment override and optional bzImage output:

```sh
FRAGMENTS="$PWD/images/guest-kernel/snd.fragment $PWD/images/guest-kernel/ninep.fragment $PWD/images/guest-kernel/qemu.fragment" \
  OUT=/tmp/vmlinux OUT_QEMU=/tmp/qemu-kernel images/guest-kernel/build.sh
```

Select the resulting bzImage with `vm ( kernel /absolute/path/to/qemu-kernel )`.
An x86 ELF kernel is rejected. The existing HTTP(S) kernel override also works.
Verify the resolved kernel configuration includes the required HDA and KVM
options before running the feature tests. Clawk does not publish QEMU feature
kernels or add a kernel publication pipeline.

## Restore limitations

`clawk snapshot` syncs and pauses the guest, saves memory and device state,
then stops QEMU. The source never runs beyond its save point. Restore checks
the host, CPU, QEMU/virtiofsd versions, device configuration, kernel, disks and
shared-file identity and contents. The checks can be expensive for large
exports. Migration has a ten-minute budget; the outer startup deadline also
allows kernel resolution, preparation, guest readiness and cleanup.

Host files remain authoritative. Changed worktrees, agent state, kernels,
disks or configuration invalidate saved memory. The CLI consumes the snapshot
once and cold-boots the existing disk if backend restore fails. Changes to
the embedded guest sources or configured image discard saved memory before
disk preparation and start from a fresh image clone. QEMU also discards
older snapshots that lack this guest-source metadata. Processes held in
invalidated memory are lost. Live snapshots likewise become unusable when the
source changes its disks or shares. Avoid concurrent host edits during a
snapshot or restore; consistency checks do not lock other applications out.
Exported filesystems must also support virtiofsd's migration of open files.
Re-exporting a host virtio-fs mount can fail restoration because its file handles
cannot be serialized; use local host filesystems for workloads needing restore.

A saved CID occupied by another VMM prevents memory restoration. Guest
services reconnect and pass identity/mount checks before the public agent
socket appears. Native Linux vsock services authenticate a private per-sandbox
token in addition to the host Unix socket's permissions.

Normal cold boots clone the current OCI image with the current guest binaries,
just as VZ does. Only host shares and host-backed agent state persist across
cold boots; install enduring tools in the image.

Old QEMU disks predating the disposable-rootfs format (version 4) require
recreation, including disks with an earlier version marker.
Save guest-only files first. Normal destruction checks dirty worktrees and
preserves host-backed agent state.

## Diagnose and validate

Start with `clawk doctor --provider qemu` for host prerequisites or
`clawk doctor <sandbox>` for an existing sandbox. Startup
errors include daemon and console tails. The sandbox's VM directory contains
`qemud.log`, `console.log` and backend QEMU/virtiofsd logs. A failed startup
stops its daemon and helpers; SIGINT/SIGTERM cancel preparation and downloads.

Reproducible Linux amd64 default-kernel, optional PCM/nested-KVM and disk-full
checks are listed in [QEMU validation](qemu-validation.md).
