//go:build !linux || !amd64

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr,
		"smoke-firecracker requires Linux amd64 with /dev/kvm. "+
			"Run this on an x86-64 KVM host.")
	os.Exit(1)
}
