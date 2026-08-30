//go:build !linux || !amd64

package firecracker

// No init on non-linux; machine.Get(Name) returns machine.ErrNoBackend.
