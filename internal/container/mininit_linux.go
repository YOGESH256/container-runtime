//go:build linux

package container

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// runMiniInit keeps a tiny init at PID 1 and runs the command as its child.
//
// PID 1 has two special duties. Orphaned processes are re-parented to it,
// so it must wait() on them or they stay as zombies. And the kernel drops
// any signal sent to PID 1 that it has no handler for, so a program that
// never expected to be PID 1 may ignore SIGTERM and Ctrl-C. This init
// handles both: it reaps everything and relays signals to the command.
// It is the same job tini does for `docker run --init`.
func runMiniInit(cfg *Config) int {
	sigs := make(chan os.Signal, 128)
	signal.Notify(sigs) // every catchable signal, SIGCHLD included

	u, err := resolveUser(cfg.User)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mycrt: %v\n", err)
		return 125
	}
	env := withDefaultHome(cfg.Env, u.home)
	path, err := lookPath(cfg.Args[0], env)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mycrt: %v\n", err)
		return 127
	}
	groups := make([]uint32, len(u.groups))
	for i, g := range u.groups {
		groups[i] = uint32(g)
	}
	// The init stays root inside the container, with the kept
	// capabilities and the seccomp filter. The command is forked from
	// this thread and inherits both; its own setuid below drops the
	// capabilities entirely for a non-root user.
	if err := hardenPre(); err != nil {
		fmt.Fprintf(os.Stderr, "mycrt: %v\n", err)
		return 125
	}
	if err := hardenPost(true); err != nil {
		fmt.Fprintf(os.Stderr, "mycrt: %v\n", err)
		return 125
	}
	tty := isTerminal(os.Stdin)
	unix.Umask(0o022)
	child, err := syscall.ForkExec(path, cfg.Args, &syscall.ProcAttr{
		Dir:   cfg.Cwd,
		Env:   env,
		Files: []uintptr{0, 1, 2},
		Sys: &syscall.SysProcAttr{
			Setpgid: true,
			// On a terminal, hand the foreground to the command's process
			// group, so Ctrl-C goes straight to it instead of to init.
			Foreground: tty,
			Ctty:       0,
			Credential: &syscall.Credential{Uid: uint32(u.uid), Gid: uint32(u.gid), Groups: groups},
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mycrt: exec %s: %v\n", path, err)
		if err == syscall.ENOENT {
			return 127
		}
		return 126
	}

	for s := range sigs {
		switch sig := s.(syscall.Signal); sig {
		case syscall.SIGCHLD:
			if code, exited := reapAll(child); exited {
				// Exiting PID 1 makes the kernel kill everything left
				// in the namespace, so nothing outlives the command.
				return code
			}
		case syscall.SIGURG:
			// Go's scheduler sends itself SIGURG to preempt goroutines.
		default:
			syscall.Kill(child, sig)
		}
	}
	return 0
}

// reapAll collects every exited child, orphans included, without
// blocking. It reports the command's exit code once the command is gone.
func reapAll(command int) (code int, exited bool) {
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
		if err != nil || pid <= 0 {
			return code, exited
		}
		if pid == command {
			code, exited = exitCode(syscall.WaitStatus(ws)), true
		}
	}
}
