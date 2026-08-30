//go:build linux && amd64

// A dependency-free Linux guest probe. Built for the guest architecture by
// the integration test; testdata keeps it out of release binaries.
package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var le = binary.LittleEndian
var pins runtime.Pinner

func main() {
	defer pins.Unpin()
	mode := "ssh"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	limit := 15 * time.Second
	if mode == "nested-persist" || mode == "http-serve" {
		limit = 4 * time.Minute
	}
	time.AfterFunc(limit, func() { fmt.Fprintln(os.Stderr, "probe timed out"); os.Exit(1) })
	switch mode {
	case "ssh":
		ssh()
	case "audio":
		capture()
	case "nested", "nested-persist":
		nested(mode == "nested-persist")
	case "nested-start":
		log, err := os.Create("/tmp/nested.log")
		check(err)
		defer log.Close()
		cmd := exec.Command(os.Args[0], "nested-persist")
		cmd.Stdout, cmd.Stderr = log, log
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		check(cmd.Start())
		check(cmd.Process.Release())
	case "http-start":
		log, err := os.Create("/tmp/http-probe.log")
		check(err)
		defer log.Close()
		cmd := exec.Command(os.Args[0], "http-serve", os.Args[2])
		cmd.Stdout, cmd.Stderr = log, log
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		check(cmd.Start())
		check(cmd.Process.Release())
	case "http-serve":
		check(http.ListenAndServe(":18080", http.FileServer(http.Dir(os.Args[2]))))
	case "no-nested":
		f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
		if err == nil {
			f.Close()
			panic("nested KVM exposed while disabled")
		}
		fmt.Println("nested KVM disabled")
	default:
		panic("unknown probe")
	}
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}
func ioctl(fd, op, arg uintptr) uintptr {
	r, _, err := syscall.Syscall(syscall.SYS_IOCTL, fd, op, arg)
	if err != 0 {
		panic(fmt.Sprintf("ioctl %#x: %v", op, err))
	}
	return r
}
func ptr(b []byte) uintptr {
	// Some UAPI structs contain pointers encoded as integers in byte buffers.
	// Pin their backing objects until every ioctl has finished.
	pins.Pin(&b[0])
	return uintptr(unsafe.Pointer(&b[0]))
}
func ssh() {
	c, err := net.DialTimeout("unix", "/run/clawk-ssh-agent.sock", 5*time.Second)
	check(err)
	defer c.Close()
	check(c.SetDeadline(time.Now().Add(5 * time.Second)))
	_, err = c.Write([]byte{0, 0, 0, 1, 11})
	check(err)
	b := make([]byte, 9)
	_, err = io.ReadFull(c, b)
	check(err)
	if binary.BigEndian.Uint32(b) != 5 || b[4] != 12 {
		panic("invalid SSH agent response")
	}
	fmt.Println("ssh agent reached")
}

// ALSA UAPI layouts below target Linux amd64. No libasound or package installation in the guest is needed.
func capture() {
	ctl, err := os.OpenFile("/dev/snd/controlC0", os.O_RDWR, 0)
	check(err)
	defer ctl.Close()
	ids := make([]byte, 64*128)
	list := make([]byte, 80)
	le.PutUint32(list[4:], 128)
	le.PutUint64(list[16:], uint64(ptr(ids)))
	ioctl(ctl.Fd(), 0xc0505510, ptr(list)) // ELEM_LIST
	for i := uint32(0); i < le.Uint32(list[8:]); i++ {
		id := ids[i*64 : (i+1)*64]
		if !strings.Contains(string(id[16:60]), "Capture") {
			continue
		}
		info := make([]byte, 272)
		copy(info, id)
		ioctl(ctl.Fd(), 0xc1105511, ptr(info)) // ELEM_INFO
		kind, count := le.Uint32(info[64:]), le.Uint32(info[72:])
		if (kind != 1 && kind != 2) || count > 128 || le.Uint32(info[68:])&2 == 0 {
			continue
		}
		value := make([]byte, 1224)
		copy(value, id)
		for n := uint32(0); n < count; n++ {
			le.PutUint64(value[72+8*n:], le.Uint64(info[88:]))
		}
		ioctl(ctl.Fd(), 0xc4c85513, ptr(value)) // ELEM_WRITE
	}
	runtime.KeepAlive(ids)
	f, err := os.OpenFile("/dev/snd/pcmC0D0c", os.O_RDWR, 0)
	check(err)
	defer f.Close()
	hw := make([]byte, 608)
	for i, bit := range []uint32{3, 2, 0} {
		le.PutUint32(hw[4+32*i:], 1<<bit)
	} // RW_INTERLEAVED, S16_LE, STD
	for i := 0; i < 12; i++ {
		le.PutUint32(hw[264+12*i:], ^uint32(0))
	}
	for param, value := range map[int]uint32{10: 2, 11: 48000, 13: 1024, 15: 4} {
		o := 260 + 12*(param-8)
		le.PutUint32(hw[o:], value)
		le.PutUint32(hw[o+4:], value)
		le.PutUint32(hw[o+8:], 4)
	}
	le.PutUint32(hw[512:], ^uint32(0))
	ioctl(f.Fd(), 0xc2604111, ptr(hw)) // HW_PARAMS
	sw := make([]byte, 136)
	le.PutUint32(sw[4:], 1)
	for off, value := range map[int]uint64{16: 1024, 24: 1, 32: 1, 40: 4096, 64: 1 << 62} {
		le.PutUint64(sw[off:], value)
	}
	ioctl(f.Fd(), 0xc0884113, ptr(sw)) // SW_PARAMS
	ioctl(f.Fd(), 0x4140, 0)           // PREPARE
	buf := make([]byte, 4096)
	xfer := make([]byte, 24)
	var samples []float64
	xruns := 0
	for len(samples) < 96000 {
		le.PutUint64(xfer[8:], uint64(ptr(buf)))
		le.PutUint64(xfer[16:], 1024)
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), 0x80184151, ptr(xfer)) // READI_FRAMES
		if errno == syscall.EPIPE && xruns < 5 {
			// ALSA capture can overrun while the host runs other VM tests.
			// Recover as an ALSA client does, then require a full fresh sample.
			xruns++
			ioctl(f.Fd(), 0x4140, 0) // PREPARE
			samples = samples[:0]
			continue
		}
		if errno != 0 {
			panic(fmt.Sprintf("PCM read: %v", errno))
		}
		n := int64(le.Uint64(xfer))
		if n < 0 || n > 1024 {
			panic(fmt.Sprintf("PCM read: %d", n))
		}
		for i := 0; i < int(n); i++ {
			samples = append(samples, float64(int16(le.Uint16(buf[4*i:])))/32768)
		}
	}
	runtime.KeepAlive(buf)
	samples = samples[24000:] // discard startup latency
	var energy float64
	crossings := 0
	for i, x := range samples {
		energy += x * x
		if i > 0 && samples[i-1] < 0 && x >= 0 {
			crossings++
		}
	}
	rms := math.Sqrt(energy / float64(len(samples)))
	hz := float64(crossings) * 48000 / float64(len(samples))
	fmt.Printf("PCM capture: RMS=%.3f frequency=%.1fHz samples=%d\n", rms, hz, len(samples))
	if rms < 0.01 || hz < 420 || hz > 460 {
		panic("440Hz host tone missing from guest capture")
	}
}

// Execute a minimal inner guest and verify its I/O exit. This proves KVM_RUN
// actually enters guest code, rather than merely checking /dev/kvm exists.
func nested(persist bool) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	check(err)
	defer f.Close()
	if ioctl(f.Fd(), 0xae00, 0) != 12 {
		panic("KVM API version")
	}
	vm := int(ioctl(f.Fd(), 0xae01, 0))
	defer syscall.Close(vm)
	ram, err := syscall.Mmap(-1, 0, 4096, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	check(err)
	defer syscall.Munmap(ram)
	gpa := uint64(0)
	region := make([]byte, 32)
	le.PutUint64(region[8:], gpa)
	le.PutUint64(region[16:], 4096)
	le.PutUint64(region[24:], uint64(ptr(ram)))
	ioctl(uintptr(vm), 0x4020ae46, ptr(region)) // SET_USER_MEMORY_REGION
	cpu := int(ioctl(uintptr(vm), 0xae41, 0))
	defer syscall.Close(cpu)
	run, err := syscall.Mmap(cpu, 0, int(ioctl(f.Fd(), 0xae04, 0)), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	check(err)
	defer syscall.Munmap(run)

	copy(ram, []byte{0xb0, 0x4b, 0xe6, 0xe9, 0xb0, 0x52, 0xe6, 0xe9, 0xf4}) // mov al,'K'; out 0xe9,al; hlt
	sregs := make([]byte, 312)
	ioctl(uintptr(cpu), 0x8138ae83, ptr(sregs))
	le.PutUint64(sregs, 0)
	le.PutUint16(sregs[12:], 0)
	ioctl(uintptr(cpu), 0x4138ae84, ptr(sregs))
	regs := make([]byte, 144)
	le.PutUint64(regs[136:], 2)
	ioctl(uintptr(cpu), 0x4090ae82, ptr(regs))
	runInner := func(want byte) {
		for {
			_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(cpu), 0xae80, 0)
			if errno == syscall.EINTR {
				continue
			}
			if errno != 0 {
				panic(errno)
			}
			break
		}
		exit := le.Uint32(run[8:])
		if exit != 2 || le.Uint16(run[34:]) != 0xe9 || run[le.Uint64(run[40:])] != want {
			panic(fmt.Sprintf("unexpected KVM exit %d", exit))
		}
	}
	runInner('K')
	fmt.Println("inner guest executed through KVM_RUN")
	if persist {
		check(os.WriteFile("/tmp/nested-ready", []byte("ready"), 0600))
		for {
			if _, err := os.Stat("/tmp/nested-continue"); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		runInner('R')
		check(os.WriteFile("/tmp/nested-resumed", []byte("inner guest resumed through KVM_RUN"), 0600))
	}
}
