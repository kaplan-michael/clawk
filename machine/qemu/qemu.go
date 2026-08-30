//go:build linux && amd64

// Package qemu provides the Linux amd64 KVM QEMU microvm backend.
// Import it for the side effect of registering the "qemu" machine backend.
package qemu

// Name is the registered backend name.
const Name = "qemu"

// GuestArch is the architecture supported by the microvm backend.
func GuestArch() string { return "amd64" }
