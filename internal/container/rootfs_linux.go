//go:build linux

package container

import (
	"errors"
	"fmt"
	"strings"

	"github.com/YOGESH256/container-runtime/internal/fsutil"

	"golang.org/x/sys/unix"
)

const (
	nosuidNoexecNodev = unix.MS_NOSUID | unix.MS_NOEXEC | unix.MS_NODEV
)

type mountSpec struct {
	target, fstype, source string
	flags                  uintptr
	data                   string
}

// setupRootfs builds the container's filesystem view under cfg.Rootfs.
// It runs in the child's private mount namespace, so every mount here
// vanishes automatically when the container's last process exits.
func setupRootfs(cfg *Config) error {
	// Mounts marked "shared" pass mount and unmount events to their peers.
	// systemd marks / shared, and our new namespace starts as a copy of the
	// host's, so without this our mounts would appear on the host.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mounts private: %w", err)
	}

	if len(cfg.Lowerdirs) > 0 {
		opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
			strings.Join(cfg.Lowerdirs, ":"), cfg.Upperdir, cfg.Workdir)
		// userxattr: mounted from inside a user namespace, overlayfs keeps
		// its bookkeeping in user.overlay.* attributes, since trusted.*
		// ones need real root. volatile: when an overlay mount goes away,
		// the kernel syncs its upper filesystem, which here is the host's
		// whole root disk. That made every container exit take hundreds of
		// milliseconds. volatile skips those syncs. The price is that the
		// upper layer can't be reused after a host crash, and mycrt never
		// reuses one anyway.
		opts += ",userxattr"
		err := unix.Mount("overlay", cfg.Rootfs, "overlay", 0, opts+",volatile")
		if errors.Is(err, unix.EINVAL) { // kernels before 5.11
			err = unix.Mount("overlay", cfg.Rootfs, "overlay", 0, opts)
		}
		if err != nil {
			return fmt.Errorf("mount overlay: %w", err)
		}
	} else {
		// pivot_root needs the new root to be a mount point.
		if err := unix.Mount(cfg.Rootfs, cfg.Rootfs, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("bind rootfs: %w", err)
		}
	}

	root, err := unix.Open(cfg.Rootfs, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open rootfs: %w", err)
	}
	defer unix.Close(root)

	mounts := []mountSpec{
		// A proc mounted from inside the new PID namespace lists only
		// the container's processes.
		{"/proc", "proc", "proc", nosuidNoexecNodev, ""},
		{"/sys", "sysfs", "sysfs", nosuidNoexecNodev | unix.MS_RDONLY, ""},
		{"/dev", "tmpfs", "tmpfs", unix.MS_NOSUID | unix.MS_STRICTATIME, "mode=755,size=65536k"},
		{"/dev/pts", "devpts", "devpts", unix.MS_NOSUID | unix.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620,gid=5"},
		{"/dev/shm", "tmpfs", "shm", nosuidNoexecNodev, "mode=1777,size=65536k"},
		{"/dev/mqueue", "mqueue", "mqueue", nosuidNoexecNodev, ""},
	}
	if cfg.CgroupNS {
		mounts = append(mounts, mountSpec{"/sys/fs/cgroup", "cgroup2", "cgroup2", nosuidNoexecNodev | unix.MS_RDONLY, ""})
	}
	for _, m := range mounts {
		if err := mountInRoot(root, m); err != nil {
			return err
		}
		if m.target == "/dev" {
			if err := populateDev(root); err != nil {
				return err
			}
		}
	}
	for target, source := range cfg.Files {
		if err := bindFileInRoot(root, target, source); err != nil {
			return err
		}
	}
	return maskPaths(root)
}

// Paths under /proc and /sys that expose or steer the host kernel. They
// are covered with /dev/null or an empty read-only tmpfs, so they exist
// but say nothing. The list matches Docker's.
var maskedPaths = []string{
	"/proc/acpi", "/proc/asound", "/proc/interrupts", "/proc/kcore", "/proc/keys",
	"/proc/latency_stats", "/proc/sched_debug", "/proc/scsi", "/proc/timer_list",
	"/proc/timer_stats", "/sys/firmware", "/sys/devices/virtual/powercap",
}

// readonlyPaths change kernel settings when written to.
var readonlyPaths = []string{
	"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger",
}

func maskPaths(root int) error {
	for _, p := range maskedPaths {
		fd, err := fsutil.OpenInRoot(root, p, unix.O_PATH)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return fmt.Errorf("mask %s: %w", p, err)
		}
		var st unix.Stat_t
		unix.Fstat(fd, &st)
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			err = unix.Mount("tmpfs", fsutil.ProcPath(fd), "tmpfs", unix.MS_RDONLY|nosuidNoexecNodev, "size=4k,mode=755")
		} else {
			err = unix.Mount("/dev/null", fsutil.ProcPath(fd), "", unix.MS_BIND, "")
		}
		unix.Close(fd)
		if err != nil {
			return fmt.Errorf("mask %s: %w", p, err)
		}
	}
	for _, p := range readonlyPaths {
		fd, err := fsutil.OpenInRoot(root, p, unix.O_PATH)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read-only %s: %w", p, err)
		}
		target := fsutil.ProcPath(fd)
		err = unix.Mount(target, target, "", unix.MS_BIND, "")
		unix.Close(fd)
		if err != nil {
			return fmt.Errorf("read-only %s: %w", p, err)
		}
		// The old handle still points under the new bind mount; look the
		// path up again to land on top of it.
		if fd, err = fsutil.OpenInRoot(root, p, unix.O_PATH); err != nil {
			return fmt.Errorf("read-only %s: %w", p, err)
		}
		// A remount inside a user namespace may only add restrictions, so
		// repeat the flags the parent mount already has.
		err = unix.Mount("", fsutil.ProcPath(fd), "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY|nosuidNoexecNodev, "")
		unix.Close(fd)
		if err != nil {
			return fmt.Errorf("read-only %s: %w", p, err)
		}
	}
	return nil
}

func mountInRoot(root int, m mountSpec) error {
	fd, err := fsutil.MkdirAllInRoot(root, m.target, 0o755)
	if err != nil {
		return fmt.Errorf("mountpoint %s: %w", m.target, err)
	}
	defer unix.Close(fd)
	if err := unix.Mount(m.source, fsutil.ProcPath(fd), m.fstype, m.flags, m.data); err != nil {
		return fmt.Errorf("mount %s on %s: %w", m.fstype, m.target, err)
	}
	return nil
}

func bindFileInRoot(root int, target, source string) error {
	fd, err := fsutil.CreateFileInRoot(root, target, 0o644)
	if err != nil {
		return fmt.Errorf("bind target %s: %w", target, err)
	}
	defer unix.Close(fd)
	if err := unix.Mount(source, fsutil.ProcPath(fd), "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind %s: %w", target, err)
	}
	return nil
}

// populateDev fills the fresh /dev tmpfs with the handful of device nodes
// and symlinks programs expect. The nodes are bind mounts of the host's,
// because a user namespace may not create device nodes, and that is a
// feature: the container can only ever reach the devices listed here.
func populateDev(root int) error {
	for _, name := range []string{"null", "zero", "full", "random", "urandom", "tty"} {
		fd, err := fsutil.CreateFileInRoot(root, "/dev/"+name, 0o666)
		if err != nil {
			return fmt.Errorf("/dev/%s: %w", name, err)
		}
		err = unix.Mount("/dev/"+name, fsutil.ProcPath(fd), "", unix.MS_BIND, "")
		unix.Close(fd)
		if err != nil {
			return fmt.Errorf("bind /dev/%s: %w", name, err)
		}
	}
	dev, err := fsutil.OpenInRoot(root, "/dev", unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return fmt.Errorf("open /dev: %w", err)
	}
	defer unix.Close(dev)
	links := []struct{ target, name string }{
		{"/proc/self/fd", "fd"},
		{"/proc/self/fd/0", "stdin"},
		{"/proc/self/fd/1", "stdout"},
		{"/proc/self/fd/2", "stderr"},
		{"pts/ptmx", "ptmx"},
	}
	for _, l := range links {
		if err := unix.Symlinkat(l.target, dev, l.name); err != nil {
			return fmt.Errorf("symlink /dev/%s: %w", l.name, err)
		}
	}
	return nil
}

// pivotRoot makes newRoot the root of this mount namespace and detaches
// the old root entirely. Unlike chroot, nothing of the host filesystem
// stays reachable afterwards, not even through ".." or an open fd trick.
func pivotRoot(newRoot string) error {
	if err := unix.Chdir(newRoot); err != nil {
		return fmt.Errorf("chdir to rootfs: %w", err)
	}
	// pivot_root(".", ".") stacks the old root on top of the new one at
	// "." (see NOTES in pivot_root(2)). Detaching the top mount at "."
	// then leaves only the new root, with no temporary directory needed.
	if err := unix.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("detach old root: %w", err)
	}
	return unix.Chdir("/")
}
