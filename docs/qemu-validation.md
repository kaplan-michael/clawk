# QEMU validation

QEMU runs only on Linux amd64 (x86-64). Validate the **default kernel** on that host. Custom kernels are separate opt-in checks for PCM
capture and nested KVM. Kernel publication is not a release requirement.
The QEMU provider, daemon, machine backend and kernel fetcher are excluded
from builds for every other platform.

## Module checks

Run from the repository root:

```sh
gofmt -l $(git ls-files '*.go')
go build ./...
go vet ./...
go test ./...
go test -race ./internal/cli ./internal/sandbox ./internal/vzdctl ./internal/vsockclient
(cd machine && go build ./... && go vet ./... && go test ./...)
(cd machine && go test -race ./qemu ./guest ./balloon ./kernel ./internal/usermode)
```

The guest-init tests execute account reconciliation from the embedded source:
UID/GID differences, adopted UID owners, existing target groups, duplicate
agent entries, supplementary memberships, custom homes, sudo configuration, PID 1 with an empty PATH, and repeated initialization.
The native default-image test also runs `sudo -n` and `visudo -c`, verifies
clean guest shutdown, fresh rootfs contents after a direct restart, preserves
host-backed agent state,
and rejects snapshots carrying an obsolete guest-source hash.
Guest telemetry tests retain the older wire
formats. Lifecycle checks include short injected phase budgets, immediate
daemon exits, cancellation and cleanup, consistency checks and cold fallback.

## Native Linux amd64

Use an unprivileged host meeting [the prerequisites](qemu.md#install-and-select).
The script runs guest, direct-daemon and actual provider-startup tests with
the race detector. The provider lifecycle test runs three separate sandboxes
and checks execution, persistent state, live policy reload,
forwarding, pause/resume, suspend/restore, failed-start cleanup, idle shutdown
and destruction.

```sh
TEST_QEMU_KERNEL= QEMU_TEST_FEATURES=0 scripts/test-qemu-linux.sh
```

For real ENOSPC injection, use a private 1 MiB filesystem; do not fill the
host's general-purpose filesystem:

```sh
qemu_small_fs="$(mktemp -d)"
sudo mount -t tmpfs -o "size=1m,uid=$(id -u),gid=$(id -g),mode=700" tmpfs "$qemu_small_fs"
trap 'sudo umount "$qemu_small_fs"; rmdir "$qemu_small_fs"' EXIT
TEST_QEMU_KERNEL= QEMU_TEST_FEATURES=0 TEST_QEMU_SMALL_FS="$qemu_small_fs" \
  scripts/test-qemu-linux.sh
```

## Optional custom-kernel checks

Build the [QEMU feature kernel](qemu.md#optional-custom-kernels). Install `pactl`
and `paplay`, plus `pulseaudio` on headless runners. The fixture uses a private
440 Hz source; it does not record a physical microphone.

```sh
QEMU_TEST_FEATURES=1 TEST_QEMU_KERNEL=/absolute/path/to/qemu-kernel \
  scripts/test-qemu-linux.sh
```

This checks captured PCM before/after restore and crash recovery, nested
KVM_RUN including an inner VM surviving outer restore, and nesting disabled.
The capture probe recovers bounded ALSA overruns and still requires a full
fresh sample with the expected amplitude and frequency.

## macOS regression gate

VZ shares guest account setup, agent transport, telemetry, balloon policy and
watchdogs with QEMU, while retaining its existing startup sequence and mount
handling. Run on a native Apple silicon Mac
with Virtualization.framework support:

```sh
scripts/test-vz-macos.sh
```

The script signs its test binary and boots the actual default development
image. It checks passwordless sudo, live shares, the shared service startup,
telemetry, balloon policy and pause/resume. Run this locally on native macOS;
ordinary CI does not establish native virtualization parity.

A skipped audio, nesting or native VZ test does not establish feature parity.
Record which checks ran, which failed, and which host prerequisites were missing.
