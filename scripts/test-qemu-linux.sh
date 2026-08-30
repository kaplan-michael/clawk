#!/usr/bin/env bash
# Linux amd64 QEMU microvm integration suite.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
if [ "$(uname -s)" != Linux ] || [ "$(uname -m)" != x86_64 ]; then
  echo "QEMU microvm tests require Linux amd64 (x86-64)" >&2
  exit 1
fi
export TEST_QEMU_BOOT=1 TEST_QEMU_FAULTS=1
export TEST_QEMU_STRESS_CYCLES="${TEST_QEMU_STRESS_CYCLES:-100}"
export TEST_GUEST_BOOT_CACHE="${TEST_GUEST_BOOT_CACHE:-$HOME/.clawk/cache}"
mkdir -p "$TEST_GUEST_BOOT_CACHE"
if [ "${QEMU_TEST_FEATURES:-0}" = 1 ]; then
  : "${TEST_QEMU_KERNEL:?select the feature-enabled kernel with TEST_QEMU_KERNEL}"
  export TEST_QEMU_AUDIO_PCM=1 TEST_QEMU_NESTED=1
fi
go test -race ./internal/e2e -run '^TestQEMUGuest$' -v -count=1
go test -race -timeout 30m ./internal/cli -run '^TestQEMU(DefaultImage|DevelopmentWorkflow|ProviderLifecycle)$' -v -count=1
if [ "${TEST_QEMU_NESTED:-0}" = 1 ]; then
  TEST_QEMU_NESTED=0 TEST_QEMU_AUDIO_PCM=0 TEST_QEMU_FAULTS=0 \
    go test -race -timeout 30m ./internal/cli -run '^TestQEMUDevelopmentWorkflow$' -v -count=1
fi
(cd machine && go test -race ./qemu ./guest ./balloon ./kernel ./internal/usermode -v -count=1)
