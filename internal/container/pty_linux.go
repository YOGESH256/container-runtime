//go:build linux

package container

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// attachPTY runs in the container after pivot_root. It opens a new
// pseudo-terminal pair from the container's own /dev/pts, so the slave is
// a real path inside (/dev/pts/0), makes it the controlling terminal and
// standard input, output and error, and ships the master end to the
// parent over the socket on ptyFD.
func attachPTY() error {
	master, err := unix.Open("/dev/pts/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open /dev/pts/ptmx: %w", err)
	}
	defer unix.Close(master)
	unlock := 0
	if err := unix.IoctlSetPointerInt(master, unix.TIOCSPTLCK, unlock); err != nil {
		return fmt.Errorf("unlock pty: %w", err)
	}
	n, err := unix.IoctlGetInt(master, unix.TIOCGPTN)
	if err != nil {
		return fmt.Errorf("pty number: %w", err)
	}
	slave, err := unix.Open(fmt.Sprintf("/dev/pts/%d", n), unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open pty slave: %w", err)
	}
	defer unix.Close(slave)

	sock := os.NewFile(ptyFD, "pty-socket")
	defer sock.Close()
	rights := unix.UnixRights(master)
	if err := unix.Sendmsg(int(sock.Fd()), []byte{0}, rights, nil, 0); err != nil {
		return fmt.Errorf("send pty master: %w", err)
	}
	// Setsid happened at clone time, so this process leads a session with
	// no terminal yet; claim the slave as its controlling terminal.
	if err := unix.IoctlSetInt(slave, unix.TIOCSCTTY, 0); err != nil {
		return fmt.Errorf("set controlling terminal: %w", err)
	}
	for _, fd := range []int{0, 1, 2} {
		if err := unix.Dup2(slave, fd); err != nil {
			return fmt.Errorf("dup pty onto fd %d: %w", fd, err)
		}
	}
	return nil
}

// receiveFD reads the one file descriptor the container sends.
func receiveFD(sock *os.File) (*os.File, error) {
	buf := make([]byte, 1)
	oob := make([]byte, unix.CmsgSpace(4))
	n, oobn, _, _, err := unix.Recvmsg(int(sock.Fd()), buf, oob, 0)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("container exited before creating its terminal")
	}
	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, err
	}
	for _, m := range msgs {
		if fds, err := unix.ParseUnixRights(&m); err == nil && len(fds) == 1 {
			unix.SetNonblock(fds[0], true)
			return os.NewFile(uintptr(fds[0]), "pty-master"), nil
		}
	}
	return nil, fmt.Errorf("no file descriptor received")
}

// terminalRelay copies bytes between the user's terminal and the
// container's pty master, with the user's terminal in raw mode so every
// keystroke, including Ctrl-C, goes to the container rather than to mycrt.
type terminalRelay struct {
	master  *os.File
	in      *os.File
	saved   *unix.Termios
	done    chan struct{}
	outDone chan struct{}
}

func startTerminalRelay(master, in, out *os.File, sigs <-chan os.Signal) *terminalRelay {
	r := &terminalRelay{master: master, in: in, done: make(chan struct{}), outDone: make(chan struct{})}
	if t, err := unix.IoctlGetTermios(int(in.Fd()), unix.TCGETS); err == nil {
		r.saved = t
		raw := *t
		raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
		raw.Oflag &^= unix.OPOST
		raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
		raw.Cflag &^= unix.CSIZE | unix.PARENB
		raw.Cflag |= unix.CS8
		raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 1, 0
		unix.IoctlSetTermios(int(in.Fd()), unix.TCSETS, &raw)
		r.resize()
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		go func() {
			for {
				select {
				case <-r.done:
					signal.Stop(winch)
					return
				case <-winch:
					r.resize()
				}
			}
		}()
	}
	go func() {
		io.Copy(out, master) // ends with EIO once the container's side closes
		close(r.outDone)
	}()
	go func() {
		io.Copy(master, in)
		if r.saved == nil {
			// Piped input ran out: send end-of-file so a shell exits.
			master.Write([]byte{4})
		}
	}()
	return r
}

// resize copies the user's terminal size to the pty.
func (r *terminalRelay) resize() {
	var ws unix.Winsize
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, r.in.Fd(), unix.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws))); errno == 0 {
		unix.Syscall(unix.SYS_IOCTL, r.master.Fd(), unix.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws)))
	}
}

// stop drains the container's last output and restores the terminal.
func (r *terminalRelay) stop() {
	close(r.done)
	<-r.outDone
	r.master.Close()
	if r.saved != nil {
		unix.IoctlSetTermios(int(r.in.Fd()), unix.TCSETS, r.saved)
	}
}
