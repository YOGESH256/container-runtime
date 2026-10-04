//go:build linux && arm64

package container

import "golang.org/x/sys/unix"

const seccompArchID = unix.AUDIT_ARCH_AARCH64

var deniedSyscalls = append(commonDeniedSyscalls, []int{}...)
