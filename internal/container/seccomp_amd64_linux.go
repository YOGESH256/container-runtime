//go:build linux && amd64

package container

import "golang.org/x/sys/unix"

const seccompArchID = unix.AUDIT_ARCH_X86_64

var deniedSyscalls = append(commonDeniedSyscalls, []int{
	unix.SYS_IOPERM, unix.SYS_IOPL, unix.SYS_MODIFY_LDT, unix.SYS_USELIB,
	unix.SYS_SYSFS, unix.SYS__SYSCTL, unix.SYS_USTAT, unix.SYS_CREATE_MODULE,
	unix.SYS_GET_KERNEL_SYMS, unix.SYS_QUERY_MODULE,
}...)
