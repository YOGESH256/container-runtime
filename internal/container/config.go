// Package container starts a process inside fresh Linux namespaces,
// assembles its root filesystem and runs the requested command.
//
// The work is split across two processes:
//
//   - The parent (the mycrt command) creates the child with clone flags for
//     new PID, mount, UTS, IPC and network namespaces, optionally straight
//     into a cgroup. While the child waits, the parent does host-side setup
//     such as networking, then sends a Config over a pipe.
//   - The child is the same binary re-executed through /proc/self/exe with a
//     marker in its environment. The Go runtime starts several threads
//     before main runs, and some namespace operations only work in a fresh,
//     single-purpose process, so the child begins life already inside the
//     namespaces. It mounts the root filesystem, calls pivot_root and
//     finally execs the user's command, which becomes PID 1.
package container

// Config is everything the container's init process needs. The parent
// sends it as JSON on file descriptor 3 once host-side setup is finished.
type Config struct {
	ID       string   `json:"id"`
	Hostname string   `json:"hostname"`
	Args     []string `json:"args"`
	Env      []string `json:"env"`
	Cwd      string   `json:"cwd,omitempty"`
	User     string   `json:"user,omitempty"`

	// Init puts a minimal init at PID 1 that forwards signals to the
	// command and reaps orphaned processes, like `docker run --init`.
	Init bool `json:"init,omitempty"`

	// Rootfs is the directory that becomes "/". With Lowerdirs set it is
	// the overlay mount point; otherwise it is a plain unpacked rootfs that
	// gets bind-mounted onto itself.
	Rootfs    string   `json:"rootfs"`
	Lowerdirs []string `json:"lowerdirs,omitempty"` // top-most layer first
	Upperdir  string   `json:"upperdir,omitempty"`
	Workdir   string   `json:"workdir,omitempty"`

	// Files maps a path inside the container to a host file that is
	// bind-mounted over it, for example a generated /etc/resolv.conf.
	Files map[string]string `json:"files,omitempty"`

	// CgroupNS gives the container its own cgroup namespace and mounts
	// cgroup2 read-only at /sys/fs/cgroup, so it sees its own limits there.
	CgroupNS bool `json:"cgroupns,omitempty"`

	// Terminal gives the container a pseudo-terminal of its own, created
	// inside its /dev/pts. The init process sends the master end back to
	// the parent, which relays it to the user's terminal.
	Terminal bool `json:"terminal,omitempty"`

	// Debug turns on timing output in the init process, which does not
	// inherit the parent's environment.
	Debug bool `json:"debug,omitempty"`
}
