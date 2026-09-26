//go:build linux

package container

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"container-runtime/internal/debug"

	"golang.org/x/sys/unix"
)

// initEnv marks the re-executed copy of the binary that acts as the
// container's init instead of parsing the command line.
const initEnv = "_MYCRT_INIT"

// configFD is where the child finds the pipe carrying its Config.
const configFD = 3

// namespaces is the set of fresh namespaces every container gets.
const namespaces = unix.CLONE_NEWPID | unix.CLONE_NEWNS | unix.CLONE_NEWUTS |
	unix.CLONE_NEWIPC | unix.CLONE_NEWNET

// Process describes one container to start.
type Process struct {
	Config                Config
	Stdin, Stdout, Stderr *os.File

	// CgroupFD, if >= 0, is an open cgroup v2 directory. The child is
	// created directly inside it (clone3 with CLONE_INTO_CGROUP), so it
	// never runs for a moment without its limits.
	CgroupFD int

	// Setup runs once the child's namespaces exist but before its init
	// continues. It receives the child's PID as seen from the host.
	Setup func(pid int) error

	// Started runs after the container has been released to run.
	Started func(pid int)

	// Signals, if set, delivers signals to relay to the container. The
	// caller subscribes early, so a signal that arrives during setup is
	// queued and relayed once the container runs instead of killing mycrt
	// before it can clean up. If nil, Run subscribes itself.
	Signals <-chan os.Signal

	// StopSignal replaces SIGTERM when mycrt forwards a stop request.
	// Images declare one when their program expects something else,
	// for example nginx uses SIGQUIT for a graceful shutdown.
	StopSignal syscall.Signal
}

// Run starts the container and waits for its PID 1 to exit. It returns
// that exit code, or 128+N if PID 1 was killed by signal N.
func Run(p *Process) (int, error) {
	cfgR, cfgW, err := os.Pipe()
	if err != nil {
		return -1, err
	}
	defer cfgW.Close()

	cmd := &exec.Cmd{
		Path:       "/proc/self/exe",
		Args:       []string{"mycrt-init"},
		Env:        []string{initEnv + "=1"},
		Stdin:      p.Stdin,
		Stdout:     p.Stdout,
		Stderr:     p.Stderr,
		ExtraFiles: []*os.File{cfgR}, // becomes fd 3 in the child
		SysProcAttr: &syscall.SysProcAttr{
			Cloneflags: namespaces,
			// If mycrt dies, take the container down with it rather than
			// leaving an unsupervised process behind.
			Pdeathsig: syscall.SIGKILL,
		},
	}
	if p.CgroupFD >= 0 {
		cmd.SysProcAttr.UseCgroupFD = true
		cmd.SysProcAttr.CgroupFD = p.CgroupFD
	}

	sigs := p.Signals
	if sigs == nil {
		ch := SubscribeSignals()
		defer signal.Stop(ch)
		sigs = ch
	}

	if err := cmd.Start(); err != nil {
		cfgR.Close()
		return -1, fmt.Errorf("start container init: %w", err)
	}
	cfgR.Close()
	pid := cmd.Process.Pid
	debug.Logf("parent", "cloned init, host pid %d", pid)

	abort := func(err error) (int, error) {
		cmd.Process.Kill()
		cmd.Wait()
		return -1, err
	}
	if p.Setup != nil {
		if err := p.Setup(pid); err != nil {
			return abort(err)
		}
	}
	// Sending the config is what releases the child to continue.
	if err := json.NewEncoder(cfgW).Encode(&p.Config); err != nil {
		return abort(fmt.Errorf("send config: %w", err))
	}
	cfgW.Close()
	if p.Started != nil {
		p.Started(pid)
	}

	done := make(chan struct{})
	go forwardSignals(sigs, done, pid, p.StopSignal, isTerminal(p.Stdin), p.Stderr)
	err = cmd.Wait()
	close(done)
	debug.Logf("parent", "init reaped")

	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return -1, err
	}
	return exitCode(cmd.ProcessState.Sys().(syscall.WaitStatus)), nil
}

// SubscribeSignals starts catching the signals mycrt relays to containers.
// From this point they no longer terminate mycrt.
func SubscribeSignals() chan os.Signal {
	ch := make(chan os.Signal, 16)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP,
		syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2)
	return ch
}

// forwardSignals relays signals sent to mycrt to the container's PID 1.
//
// When stdin is a terminal, Ctrl-C and Ctrl-\ reach the container directly,
// because it shares mycrt's process group on that terminal. Forwarding them
// as well would deliver them twice, so they are skipped. A PID 1 that has no
// handler ignores SIGINT, so a third Ctrl-C force-kills the container.
func forwardSignals(sigs <-chan os.Signal, done <-chan struct{}, pid int,
	stopSignal syscall.Signal, tty bool, stderr *os.File) {
	interrupts := 0
	for {
		select {
		case <-done:
			return
		case s := <-sigs:
			sig := s.(syscall.Signal)
			if tty && (sig == syscall.SIGINT || sig == syscall.SIGQUIT) {
				if sig == syscall.SIGINT {
					if interrupts++; interrupts >= 3 {
						fmt.Fprintln(stderr, "mycrt: third interrupt, killing container")
						syscall.Kill(pid, syscall.SIGKILL)
					}
				}
				continue
			}
			if sig == syscall.SIGTERM && stopSignal != 0 {
				sig = stopSignal
			}
			syscall.Kill(pid, sig)
		}
	}
}

func exitCode(ws syscall.WaitStatus) int {
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}

func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}
