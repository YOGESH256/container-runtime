//go:build linux

package container

import (
	"errors"
	"fmt"
	"strings"

	"container-runtime/internal/fsutil"

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
		// When an overlay mount goes away, the kernel syncs its upper
		// filesystem, which here is the host's whole root disk. That made
		// every container exit take hundreds of milliseconds. "volatile"
		// skips those syncs. The price is that the upper layer can't be
		// reused after a host crash, and mycrt never reuses one anyway.
		err := unix.Mount("overlay", cfg.Rootfs, "overlay", 0, opts+",volatile")
		if errors.Is(err, unix.EINVAL) { // kernels before 5.10
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
// and symlinks programs expect. The container never sees the host's /dev.
func populateDev(root int) error {
	dev, err := fsutil.OpenInRoot(root, "/dev", unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return fmt.Errorf("open /dev: %w", err)
	}
	defer unix.Close(dev)

	old := unix.Umask(0)
	defer unix.Umask(old)
	nodes := []struct {
		name         string
		major, minor uint32
	}{
		{"null", 1, 3}, {"zero", 1, 5}, {"full", 1, 7},
		{"random", 1, 8}, {"urandom", 1, 9}, {"tty", 5, 0},
	}
	for _, n := range nodes {
		if err := unix.Mknodat(dev, n.name, unix.S_IFCHR|0o666, int(unix.Mkdev(n.major, n.minor))); err != nil {
			return fmt.Errorf("mknod /dev/%s: %w", n.name, err)
		}
	}
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
