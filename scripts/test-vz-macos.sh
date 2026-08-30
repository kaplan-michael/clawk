#!/usr/bin/env bash
# Native VZ gate for shared guest services. Requires Virtualization.framework.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
if [ "$(uname -s)" != Darwin ]; then
  echo "VZ smoke tests require native macOS" >&2
  exit 1
fi
vz_smoke_dir="$(mktemp -d)"
trap 'rm -rf "$vz_smoke_dir"' EXIT
go test -c -race -o "$vz_smoke_dir/cli.test" ./internal/cli
codesign --entitlements clawk.entitlements -s - --force "$vz_smoke_dir/cli.test"
TEST_VZ_BOOT=1 "$vz_smoke_dir/cli.test" -test.run '^TestVZSharedServices$' -test.v
