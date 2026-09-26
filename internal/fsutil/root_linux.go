//go:build linux

// Package fsutil resolves paths inside a container root filesystem.
//
// A root filesystem comes from an image, so it can contain symlinks such as
// /proc -> /etc or /dev -> ../../host. If the runtime naively joined
// rootfs+"/proc" and mounted there, the kernel would follow the symlink on
// the host side. Everything here uses openat2(RESOLVE_IN_ROOT), which makes
// the kernel treat the rootfs directory as "/" while resolving, so ".." and
// absolute symlinks can never walk out of it.
package fsutil

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

// OpenInRoot opens p relative to the directory fd root, resolving every
// component as if root were "/".
func OpenInRoot(root int, p string, flags int) (int, error) {
	how := unix.OpenHow{
		Flags:   uint64(flags | unix.O_CLOEXEC),
		Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS,
	}
	for {
		fd, err := unix.Openat2(root, clean(p), &how)
		if err == unix.EINTR || err == unix.EAGAIN {
			continue // openat2 may ask for a retry when a rename races with it
		}
		return fd, err
	}
}

// MkdirAllInRoot creates directory p and any missing parents inside root
// and returns an O_PATH fd for it. A symlink on the way is followed, still
// inside root, and if its target is missing the target is created. That
// matches how a process chrooted into root would see the path.
func MkdirAllInRoot(root int, p string, perm uint32) (int, error) {
	return mkdirAll(root, clean(p), perm, 0)
}

// maxSymlinks matches the kernel's limit on symlinks followed in one lookup.
const maxSymlinks = 40

func mkdirAll(root int, p string, perm uint32, depth int) (int, error) {
	if depth > maxSymlinks {
		return -1, unix.ELOOP
	}
	if fd, err := OpenInRoot(root, p, unix.O_PATH|unix.O_DIRECTORY); err == nil {
		return fd, nil
	}
	parts := strings.Split(p, "/")
	cur := "/"
	for i, name := range parts {
		if name == "" || name == "." {
			continue
		}
		// Plain concatenation, not path.Join: ".." must be resolved by the
		// kernel against real directories, not cancelled out lexically.
		next := strings.TrimSuffix(cur, "/") + "/" + name
		if fd, err := OpenInRoot(root, next, unix.O_PATH|unix.O_DIRECTORY); err == nil {
			unix.Close(fd)
			cur = next
			continue
		}
		parent, err := OpenInRoot(root, cur, unix.O_PATH|unix.O_DIRECTORY)
		if err != nil {
			return -1, fmt.Errorf("open %s: %w", cur, err)
		}
		err = unix.Mkdirat(parent, name, perm)
		if errors.Is(err, unix.EEXIST) {
			// Something exists here but is not a reachable directory. If it
			// is a symlink to a missing path, create that path instead.
			target, lerr := readlinkat(parent, name)
			unix.Close(parent)
			if lerr != nil {
				return -1, fmt.Errorf("%s exists and is not a directory", next)
			}
			if !strings.HasPrefix(target, "/") {
				target = strings.TrimSuffix(cur, "/") + "/" + target
			}
			rest := append([]string{target}, parts[i+1:]...)
			return mkdirAll(root, strings.Join(rest, "/"), perm, depth+1)
		}
		unix.Close(parent)
		if err != nil {
			return -1, fmt.Errorf("mkdir %s: %w", next, err)
		}
		cur = next
	}
	return OpenInRoot(root, p, unix.O_PATH|unix.O_DIRECTORY)
}

func readlinkat(dir int, name string) (string, error) {
	buf := make([]byte, unix.PathMax)
	n, err := unix.Readlinkat(dir, name, buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

// CreateFileInRoot makes sure regular file p exists inside root (creating
// an empty one and its parents if needed) and returns an O_PATH fd for it.
func CreateFileInRoot(root int, p string, perm uint32) (int, error) {
	p = clean(p)
	if fd, err := OpenInRoot(root, p, unix.O_PATH); err == nil {
		return fd, nil
	}
	dir, base := path.Split(p)
	parent, err := MkdirAllInRoot(root, dir, 0o755)
	if err != nil {
		return -1, err
	}
	fd, err := unix.Openat(parent, base, unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_WRONLY|unix.O_CLOEXEC, perm)
	unix.Close(parent)
	if err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, fmt.Errorf("create %s: %w", p, err)
	}
	if err == nil {
		unix.Close(fd)
	}
	return OpenInRoot(root, p, unix.O_PATH)
}

// ProcPath returns the /proc/self/fd path for fd. Syscalls that only take
// paths, like mount(2), can target an already-resolved fd through it.
func ProcPath(fd int) string { return fmt.Sprintf("/proc/self/fd/%d", fd) }

func clean(p string) string { return path.Clean("/" + p) }
