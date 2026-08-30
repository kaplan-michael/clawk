//go:build linux && amd64

package qemu

import (
	"os"
	"strings"
)

func Console() string { return "ttyS0" }

func nestedSupported() bool {
	for _, p := range []string{"/sys/module/kvm_intel/parameters/nested", "/sys/module/kvm_amd/parameters/nested"} {
		b, _ := os.ReadFile(p)
		s := strings.TrimSpace(string(b))
		if s == "Y" || s == "1" {
			return true
		}
	}
	return false
}

func cpuModel(nested bool) string {
	if nested {
		return "host"
	}
	b, _ := os.ReadFile("/proc/cpuinfo")
	if strings.Contains(string(b), "AuthenticAMD") {
		return "host,svm=off"
	}
	return "host,vmx=off"
}

func machineOptions() string {
	return "microvm,accel=kvm,kernel-irqchip=split,acpi=on,ioapic2=off,pcie=on,usb=off,graphics=off"
}
