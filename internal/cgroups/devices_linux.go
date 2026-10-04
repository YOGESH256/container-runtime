//go:build linux

package cgroups

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// cgroup v2 has no device controller files. Device access is gated by a
// small eBPF program attached to the cgroup: the kernel runs it on every
// open and mknod of a device node by a process in the group, and the
// program answers 1 (allow) or 0 (deny). This applies to root as well,
// and the only device nodes in the container are bind mounts the runtime
// made, so together they decide exactly which devices exist for it.

// deviceRule allows one device (or every minor of a major) for the
// access bits r (2), w (4) and m (1).
type deviceRule struct {
	kind   uint32 // 1 block, 2 char
	major  uint32
	minor  int64 // -1: any
	access uint32
}

// allowedDevices is the list every container gets: the memory devices,
// random numbers, and terminals.
var allowedDevices = []deviceRule{
	{2, 1, 3, 7},    // /dev/null
	{2, 1, 5, 7},    // /dev/zero
	{2, 1, 7, 7},    // /dev/full
	{2, 1, 8, 7},    // /dev/random
	{2, 1, 9, 7},    // /dev/urandom
	{2, 5, 0, 7},    // /dev/tty
	{2, 5, 1, 7},    // /dev/console
	{2, 5, 2, 7},    // /dev/ptmx
	{2, 136, -1, 7}, // /dev/pts/*
}

// eBPF instruction encoding: 1 byte opcode, 4-bit dst and src registers,
// 16-bit offset, 32-bit immediate.
type bpfInsn struct {
	code uint8
	regs uint8
	off  int16
	imm  int32
}

func insn(code uint8, dst, src uint8, off int16, imm int32) bpfInsn {
	return bpfInsn{code: code, regs: dst | src<<4, off: off, imm: imm}
}

const (
	opLdxW   = 0x61 // dst = *(u32 *)(src + off)
	opMov64X = 0xbf // dst = src
	opMov64K = 0xb7 // dst = imm
	opRsh64K = 0x77 // dst >>= imm
	opAnd64K = 0x57 // dst &= imm
	opJneK   = 0x55 // if dst != imm goto +off
	opExit   = 0x95
)

// deviceProgram compiles allowedDevices into eBPF. The context struct is
// {access_type, major, minor}; access_type packs the access bits in its
// upper 16 bits and the device kind in the lower 16.
func deviceProgram() []bpfInsn {
	prog := []bpfInsn{
		insn(opLdxW, 2, 1, 0, 0),        // r2 = access_type
		insn(opMov64X, 3, 2, 0, 0),      // r3 = r2
		insn(opAnd64K, 3, 0, 0, 0xffff), // r3 = kind
		insn(opRsh64K, 2, 0, 0, 16),     // r2 = access bits
		insn(opLdxW, 4, 1, 4, 0),        // r4 = major
		insn(opLdxW, 5, 1, 8, 0),        // r5 = minor
	}
	for _, d := range allowedDevices {
		var rule []bpfInsn
		rule = append(rule, insn(opJneK, 3, 0, 0, int32(d.kind)))
		rule = append(rule, insn(opJneK, 4, 0, 0, int32(d.major)))
		if d.minor >= 0 {
			rule = append(rule, insn(opJneK, 5, 0, 0, int32(d.minor)))
		}
		rule = append(rule,
			insn(opMov64X, 6, 2, 0, 0),                // r6 = requested access
			insn(opAnd64K, 6, 0, 0, int32(^d.access)), // r6 &= ~allowed
			insn(opJneK, 6, 0, 0, 0),                  // any bit left: no match
			insn(opMov64K, 0, 0, 0, 1),                // r0 = 1 (allow)
			insn(opExit, 0, 0, 0, 0),
		)
		// Every failed check jumps past the rest of this rule.
		for i := range rule {
			if rule[i].code == opJneK {
				rule[i].off = int16(len(rule) - i - 1)
			}
		}
		prog = append(prog, rule...)
	}
	return append(prog,
		insn(opMov64K, 0, 0, 0, 0), // r0 = 0 (deny)
		insn(opExit, 0, 0, 0, 0),
	)
}

// Layouts of the bpf(2) attribute union for the two commands used.
type bpfProgLoadAttr struct {
	progType           uint32
	insnCnt            uint32
	insns              uint64
	license            uint64
	logLevel           uint32
	logSize            uint32
	logBuf             uint64
	kernVersion        uint32
	progFlags          uint32
	progName           [16]byte
	progIfindex        uint32
	expectedAttachType uint32
}

type bpfProgAttachAttr struct {
	targetFd     uint32
	attachBpfFd  uint32
	attachType   uint32
	attachFlags  uint32
	replaceBpfFd uint32
}

// AttachDeviceFilter loads the device program and attaches it to the
// cgroup open as cgroupDir. The attachment lives as long as the cgroup.
func AttachDeviceFilter(cgroupDir *os.File) error {
	prog := deviceProgram()
	license := []byte("GPL\x00")
	logBuf := make([]byte, 64<<10)
	load := bpfProgLoadAttr{
		progType: unix.BPF_PROG_TYPE_CGROUP_DEVICE,
		insnCnt:  uint32(len(prog)),
		insns:    uint64(uintptr(unsafe.Pointer(&prog[0]))),
		license:  uint64(uintptr(unsafe.Pointer(&license[0]))),
		logLevel: 1,
		logSize:  uint32(len(logBuf)),
		logBuf:   uint64(uintptr(unsafe.Pointer(&logBuf[0]))),
	}
	copy(load.progName[:], "mycrt_devices")
	fd, _, errno := unix.Syscall(unix.SYS_BPF, unix.BPF_PROG_LOAD, uintptr(unsafe.Pointer(&load)), unsafe.Sizeof(load))
	if errno != 0 {
		n := 0
		for n < len(logBuf) && logBuf[n] != 0 {
			n++
		}
		return fmt.Errorf("load device filter: %w: %s", errno, logBuf[:n])
	}
	defer unix.Close(int(fd))
	attach := bpfProgAttachAttr{
		targetFd:    uint32(cgroupDir.Fd()),
		attachBpfFd: uint32(fd),
		attachType:  unix.BPF_CGROUP_DEVICE,
		attachFlags: unix.BPF_F_ALLOW_MULTI,
	}
	if _, _, errno := unix.Syscall(unix.SYS_BPF, unix.BPF_PROG_ATTACH, uintptr(unsafe.Pointer(&attach)), unsafe.Sizeof(attach)); errno != 0 {
		return fmt.Errorf("attach device filter: %w", errno)
	}
	return nil
}
