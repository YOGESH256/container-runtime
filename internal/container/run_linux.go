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

	"github.com/YOGESH256/container-runtime/internal/debug"

	"golang.org/x/sys/unix"
)

// initEnv marks the re-executed copy of the binary that acts as the
// container's init instead of parsing the command line.
const initEnv = "_MYCRT_INIT"

// configFD is where the child finds the pipe carrying its Config, and
// ptyFD the socket it sends its terminal's master end back on.
const (
	configFD = 3
	ptyFD    = 4
)

// IDMapSize is how many user and group IDs a container gets. Root inside
// is IDShift outside, and so on up to IDShift+IDMapSize-1.
const IDMapSize = 65536

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

	// IDShift is the host user and group ID that root in the container
	// maps to. The container gets a user namespace of its own, so root
	// inside is an ordinary unprivileged user on the host.
	IDShift int

	// Terminal attaches the container to a pseudo-terminal of its own and
	// relays it to Stdin/Stdout, which should be the user's terminal.
	Terminal bool

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

	// With a terminal, the child's stdio is the pty it creates itself;
	// it sends the master end over this socket pair.
	var ptyParent, ptyChild *os.File
	if p.Terminal {
		fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			return -1, err
		}
		ptyParent, ptyChild = os.NewFile(uintptr(fds[0]), "pty-parent"), os.NewFile(uintptr(fds[1]), "pty-child")
		defer ptyParent.Close()
		defer ptyChild.Close()
	}
	p.Config.Terminal = p.Terminal
	stdio, err := prepareStdio(p.Stdin, p.Stdout, p.Stderr, p.IDShift, p.Terminal)
	if err != nil {
		return -1, err
	}
	defer stdio.finish()

	cmd := &exec.Cmd{
		Path:       "/proc/self/exe",
		Args:       []string{"mycrt-init"},
		Env:        []string{initEnv + "=1"},
		Stdin:      stdio.in,
		Stdout:     stdio.out,
		Stderr:     stdio.err,
		ExtraFiles: []*os.File{cfgR, ptyChild}, // fds 3 and 4 in the child
		SysProcAttr: &syscall.SysProcAttr{
			Cloneflags:  namespaces | unix.CLONE_NEWUSER,
			UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: p.IDShift, Size: IDMapSize}},
			GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: p.IDShift, Size: IDMapSize}},
			// The parent is real root, so the child may keep calling
			// setgroups within its mapped range.
			GidMappingsEnableSetgroups: true,
			// Host root is not in the map, so the child starts out as
			// "nobody" there and would lose every capability on exec.
			// Becoming uid 0 of the new namespace first keeps them.
			Credential: &syscall.Credential{Uid: 0, Gid: 0, Groups: []uint32{}},
			// If mycrt dies, take the container down with it rather than
			// leaving an unsupervised process behind.
			Pdeathsig: syscall.SIGKILL,
		},
	}
	if p.Terminal {
		// The child becomes a session leader so the pty can be its
		// controlling terminal; the parent's own terminal stays with the
		// parent, which relays keystrokes through the pty.
		cmd.SysProcAttr.Setsid = true
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
		stdio.started()
		return -1, fmt.Errorf("start container init: %w", err)
	}
	cfgR.Close()
	stdio.started()
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

	// The child's first act after setup is to send its pty master, or to
	// exit with an error. Either way this returns promptly.
	var relay *terminalRelay
	if p.Terminal {
		ptyChild.Close()
		master, err := receiveFD(ptyParent)
		if err != nil {
			cmd.Wait()
			return -1, fmt.Errorf("container terminal: %w", err)
		}
		relay = startTerminalRelay(master, p.Stdin, p.Stdout, sigs)
		defer relay.stop()
	}
	if p.Started != nil {
		p.Started(pid)
	}

	done := make(chan struct{})
	go forwardSignals(sigs, done, pid, p.StopSignal, isTerminal(p.Stdin) && !p.Terminal, p.Stderr)
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
