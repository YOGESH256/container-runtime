//go:build linux

package container

import (
	"io"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// Programs reopen their own standard streams through /proc/self/fd, for
// example nginx logs to /dev/stderr. That reopen checks the file's
// permissions, and the container's root is host user IDShift, who may not
// open a terminal or log file that belongs to host root. So each stream
// the container could not reopen is replaced by a pipe owned by IDShift,
// and the parent copies between the pipe and the real stream.

// stdioRelay holds the pipes and copiers for one container.
type stdioRelay struct {
	in, out, err *os.File // what the child gets
	closeAfter   []*os.File
	wg           sync.WaitGroup
}

// prepareStdio returns the files to hand the child. With a terminal the
// child replaces its streams with the pty anyway, so nothing is relayed.
func prepareStdio(in, out, err *os.File, shift int, terminal bool) (*stdioRelay, error) {
	r := &stdioRelay{in: in, out: out, err: err}
	if terminal {
		return r, nil
	}
	var e error
	r.in, e = r.inbound(in, shift)
	if e != nil {
		return nil, e
	}
	if r.out, e = r.outbound(out, shift); e != nil {
		return nil, e
	}
	if r.err, e = r.outbound(err, shift); e != nil {
		return nil, e
	}
	return r, nil
}

// reopenable reports whether user shift could open f again by path.
func reopenable(f *os.File, shift int) bool {
	if f == nil {
		return true
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return false
	}
	if int(st.Uid) == shift {
		return true
	}
	// World read-write, like /dev/null.
	return st.Mode&0o006 == 0o006
}

func (r *stdioRelay) inbound(in *os.File, shift int) (*os.File, error) {
	if reopenable(in, shift) {
		return in, nil
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	pr.Chown(shift, shift)
	r.closeAfter = append(r.closeAfter, pr)
	go func() {
		io.Copy(pw, in)
		pw.Close()
	}()
	return pr, nil
}

func (r *stdioRelay) outbound(out *os.File, shift int) (*os.File, error) {
	if reopenable(out, shift) {
		return out, nil
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	pw.Chown(shift, shift)
	r.closeAfter = append(r.closeAfter, pw)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		io.Copy(out, pr)
		pr.Close()
	}()
	return pw, nil
}

// started closes the parent's copies of the child's pipe ends, so the
// copiers see end-of-file when the container closes them.
func (r *stdioRelay) started() {
	for _, f := range r.closeAfter {
		f.Close()
	}
}

// finish waits for the last output to be copied out.
func (r *stdioRelay) finish() { r.wg.Wait() }
