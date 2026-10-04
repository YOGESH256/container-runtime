//go:build linux

package container

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Capabilities a container's root keeps. Linux splits root's power into
// about forty pieces; these are Docker's default set minus CAP_MKNOD,
// which a user namespace blocks anyway. Everything else, such as mounting
// filesystems, loading modules, rebooting, raw disk access and changing
// the clock, is gone for good: the bounding set is dropped, so no exec
// can bring them back.
var keepCaps = []int{
	unix.CAP_AUDIT_WRITE, unix.CAP_CHOWN, unix.CAP_DAC_OVERRIDE, unix.CAP_FOWNER,
	unix.CAP_FSETID, unix.CAP_KILL, unix.CAP_NET_BIND_SERVICE, unix.CAP_NET_RAW,
	unix.CAP_SETFCAP, unix.CAP_SETGID, unix.CAP_SETPCAP, unix.CAP_SETUID, unix.CAP_SYS_CHROOT,
}

func keepMask() uint64 {
	var m uint64
	for _, c := range keepCaps {
		m |= 1 << uint(c)
	}
	return m
}

// dropBoundingSet removes every capability not in keepCaps from the
// bounding set. A root process regains at most its bounding set on exec,
// so this is the ceiling for the container's whole lifetime.
func dropBoundingSet() error {
	keep := keepMask()
	for c := 0; c <= unix.CAP_LAST_CAP; c++ {
		if keep&(1<<uint(c)) != 0 {
			continue
		}
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(c), 0, 0, 0); err != nil {
			return fmt.Errorf("drop capability %d: %w", c, err)
		}
	}
	return nil
}

// setNoNewPrivs stops setuid and file-capability binaries from granting
// extra privilege, so sudo and friends are inert inside.
func setNoNewPrivs() error {
	return unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0)
}

// applyCapabilities sets the calling thread's capabilities to keepCaps, or
// to nothing, and clears the ambient set either way.
func applyCapabilities(keep bool) error {
	var mask uint64
	if keep {
		mask = keepMask()
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{
		{Effective: uint32(mask), Permitted: uint32(mask)},
		{Effective: uint32(mask >> 32), Permitted: uint32(mask >> 32)},
	}
	if err := unix.Capset(&hdr, &data[0]); err != nil {
		return fmt.Errorf("capset: %w", err)
	}
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fmt.Errorf("clear ambient capabilities: %w", err)
	}
	return nil
}

// harden applies everything that limits what the current thread and its
// descendants can do, in the order the kernel requires:
//
//  1. shrink the bounding set (needs CAP_SETPCAP, so before any setuid)
//  2. no_new_privs
//  3. switch user (the caller does this between steps 2 and 4)
//  4. drop to the kept capabilities
//  5. install the seccomp filter (last, because it blocks setuid-adjacent
//     calls the earlier steps need)
//
// hardenPre runs steps 1 and 2; hardenPost runs 4 and 5.
func hardenPre() error {
	if err := dropBoundingSet(); err != nil {
		return err
	}
	return setNoNewPrivs()
}

func hardenPost(root bool) error {
	if err := applyCapabilities(root); err != nil {
		return err
	}
	return installSeccomp()
}

// installSeccomp loads the system-call filter for every thread.
func installSeccomp() error {
	filter := buildSeccompFilter()
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	_, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER,
		unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&prog)))
	if errno != 0 {
		return fmt.Errorf("seccomp: %w", errno)
	}
	return nil
}

// Offsets into struct seccomp_data: the syscall number, the architecture,
// and the low 32 bits of the first argument (little-endian).
const (
	seccompNr   = 0
	seccompArch = 4
	seccompArg0 = 16
)

// namespaceFlags are the clone flags that create namespaces. A container
// must not build its own, which would be a step towards escaping.
const namespaceFlags = unix.CLONE_NEWUSER | unix.CLONE_NEWPID | unix.CLONE_NEWNS |
	unix.CLONE_NEWNET | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC | unix.CLONE_NEWCGROUP

// buildSeccompFilter returns a classic BPF program that:
//
//   - kills the process on a foreign architecture (e.g. 32-bit compat calls)
//   - answers clone3 with ENOSYS, so libc falls back to clone, whose
//     flags argument the filter can inspect
//   - refuses clone with any namespace flag
//   - refuses every system call in deniedSyscalls with EPERM
//   - allows everything else
//
// A deny list keeps compatibility high; the kept capabilities already
// make most of the denied calls fail, so this is the second lock.
func buildSeccompFilter() []unix.SockFilter {
	const (
		ld  = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
		jeq = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
		jst = unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K
		ret = unix.BPF_RET | unix.BPF_K
	)
	errno := func(e unix.Errno) uint32 { return unix.SECCOMP_RET_ERRNO | uint32(e)&unix.SECCOMP_RET_DATA }
	f := []unix.SockFilter{
		{Code: ld, K: seccompArch},
		{Code: jeq, K: seccompArchID, Jt: 1},
		{Code: ret, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: ld, K: seccompNr},
		{Code: jeq, K: unix.SYS_CLONE3, Jf: 1},
		{Code: ret, K: errno(unix.ENOSYS)},
		{Code: jeq, K: unix.SYS_CLONE, Jf: 4},
		{Code: ld, K: seccompArg0},
		{Code: jst, K: namespaceFlags, Jf: 1},
		{Code: ret, K: errno(unix.EPERM)},
		{Code: ld, K: seccompNr},
	}
	for _, nr := range deniedSyscalls {
		f = append(f,
			unix.SockFilter{Code: jeq, K: uint32(nr), Jf: 1},
			unix.SockFilter{Code: ret, K: errno(unix.EPERM)},
		)
	}
	return append(f, unix.SockFilter{Code: ret, K: unix.SECCOMP_RET_ALLOW})
}
