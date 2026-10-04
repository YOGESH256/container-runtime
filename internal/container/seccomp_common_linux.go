//go:build linux

package container

import "golang.org/x/sys/unix"

// commonDeniedSyscalls are refused on every architecture. They touch the
// host as a whole: mounts and namespaces, kernel modules, the clock, swap,
// reboot, raw memory of other processes, the kernel keyring, BPF, perf,
// and io_uring, which has had a run of kernel bugs.
var commonDeniedSyscalls = []int{
	unix.SYS_ACCT, unix.SYS_ADD_KEY, unix.SYS_ADJTIMEX, unix.SYS_BPF,
	unix.SYS_CLOCK_ADJTIME, unix.SYS_CLOCK_SETTIME, unix.SYS_DELETE_MODULE,
	unix.SYS_FANOTIFY_INIT, unix.SYS_FINIT_MODULE, unix.SYS_FSCONFIG, unix.SYS_FSMOUNT,
	unix.SYS_FSOPEN, unix.SYS_FSPICK, unix.SYS_GET_MEMPOLICY, unix.SYS_INIT_MODULE,
	unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER, unix.SYS_IO_URING_SETUP,
	unix.SYS_KCMP, unix.SYS_KEXEC_FILE_LOAD, unix.SYS_KEXEC_LOAD, unix.SYS_KEYCTL,
	unix.SYS_LOOKUP_DCOOKIE, unix.SYS_MBIND, unix.SYS_MIGRATE_PAGES, unix.SYS_MOUNT,
	unix.SYS_MOUNT_SETATTR, unix.SYS_MOVE_MOUNT, unix.SYS_MOVE_PAGES, unix.SYS_NAME_TO_HANDLE_AT,
	unix.SYS_NFSSERVCTL, unix.SYS_OPEN_BY_HANDLE_AT, unix.SYS_OPEN_TREE, unix.SYS_PERF_EVENT_OPEN,
	unix.SYS_PIDFD_GETFD, unix.SYS_PIVOT_ROOT, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV,
	unix.SYS_PTRACE, unix.SYS_QUOTACTL, unix.SYS_REBOOT, unix.SYS_REQUEST_KEY,
	unix.SYS_SETDOMAINNAME, unix.SYS_SETHOSTNAME, unix.SYS_SET_MEMPOLICY, unix.SYS_SETNS,
	unix.SYS_SETTIMEOFDAY, unix.SYS_SWAPOFF, unix.SYS_SWAPON, unix.SYS_SYSLOG,
	unix.SYS_UMOUNT2, unix.SYS_UNSHARE, unix.SYS_USERFAULTFD, unix.SYS_VHANGUP,
}
