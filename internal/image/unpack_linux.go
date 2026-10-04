//go:build linux

package image

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/YOGESH256/container-runtime/internal/fsutil"

	"golang.org/x/sys/unix"
)

const (
	whiteoutPrefix = ".wh."
	whiteoutMeta   = ".wh..wh."
	whiteoutOpaque = ".wh..wh..opq"
)

// Unpack extracts one layer tarball into dir.
//
// Each layer gets its own directory and overlayfs stacks them later, so a
// deletion recorded in this layer cannot be applied by deleting a file:
// the file lives in a lower layer's directory. OCI records deletions as
// whiteout entries, and they are converted into the markers overlayfs
// reads while stacking:
//
//	.wh.NAME      becomes NAME as a 0:0 character device, hiding NAME below
//	.wh..wh..opq  becomes trusted.overlay.opaque=y on its directory,
//	              hiding everything lower layers had in it
//
// Every path is resolved with dir as its root, so entries such as
// "../../etc/passwd", or entries under a symlink pointing at "/", cannot
// write outside the layer.
func Unpack(r io.Reader, dir string) error {
	root, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(root)

	type dirTime struct {
		name  string
		mtime time.Time
	}
	var dirs []dirTime
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read layer: %w", err)
		}
		name := path.Clean("/" + hdr.Name)
		if name == "/" {
			continue
		}
		if err := unpackEntry(root, name, hdr, tr); err != nil {
			return fmt.Errorf("unpack %s: %w", name, err)
		}
		if hdr.Typeflag == tar.TypeDir {
			dirs = append(dirs, dirTime{name, hdr.ModTime})
		}
	}
	// Creating entries inside a directory bumps its mtime, so directory
	// times are applied once everything is in place.
	for _, d := range dirs {
		fd, err := fsutil.OpenInRoot(root, d.name, unix.O_PATH|unix.O_DIRECTORY)
		if err != nil {
			continue
		}
		if ts, ok := timespecs(d.mtime, d.mtime); ok {
			unix.UtimesNanoAt(unix.AT_FDCWD, fsutil.ProcPath(fd), ts, 0)
		}
		unix.Close(fd)
	}
	return nil
}

func unpackEntry(root int, name string, hdr *tar.Header, r io.Reader) error {
	dir, base := path.Split(name)
	parent, err := fsutil.MkdirAllInRoot(root, dir, 0o755)
	if err != nil {
		return err
	}
	defer unix.Close(parent)

	if strings.HasPrefix(base, whiteoutPrefix) {
		switch {
		case base == whiteoutOpaque:
			return unix.Setxattr(fsutil.ProcPath(parent), "trusted.overlay.opaque", []byte("y"), 0)
		case strings.HasPrefix(base, whiteoutMeta):
			return nil // other .wh..wh. names are aufs bookkeeping
		}
		hidden := strings.TrimPrefix(base, whiteoutPrefix)
		if err := removeEntry(parent, hidden); err != nil {
			return err
		}
		return unix.Mknodat(parent, hidden, unix.S_IFCHR, 0)
	}

	mode := uint32(hdr.Mode) & 0o7777
	switch hdr.Typeflag {
	case tar.TypeDir:
		var st unix.Stat_t
		if unix.Fstatat(parent, base, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT != unix.S_IFDIR {
			if err := removeEntry(parent, base); err != nil {
				return err
			}
		}
		if err := unix.Mkdirat(parent, base, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
	case tar.TypeReg:
		if err := removeEntry(parent, base); err != nil {
			return err
		}
		fd, err := unix.Openat(parent, base, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err != nil {
			return err
		}
		f := os.NewFile(uintptr(fd), name)
		_, err = io.Copy(f, r)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	case tar.TypeSymlink:
		if err := removeEntry(parent, base); err != nil {
			return err
		}
		if err := unix.Symlinkat(hdr.Linkname, parent, base); err != nil {
			return err
		}
	case tar.TypeLink:
		// A hard link shares the target's inode, owner and mode, so there
		// is nothing more to set afterwards.
		tdir, tbase := path.Split(path.Clean("/" + hdr.Linkname))
		tparent, err := fsutil.OpenInRoot(root, tdir, unix.O_PATH|unix.O_DIRECTORY)
		if err != nil {
			return fmt.Errorf("hard link target %s: %w", hdr.Linkname, err)
		}
		defer unix.Close(tparent)
		if err := removeEntry(parent, base); err != nil {
			return err
		}
		return unix.Linkat(tparent, tbase, parent, base, 0)
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		kind := map[byte]uint32{tar.TypeChar: unix.S_IFCHR, tar.TypeBlock: unix.S_IFBLK, tar.TypeFifo: unix.S_IFIFO}[hdr.Typeflag]
		if err := removeEntry(parent, base); err != nil {
			return err
		}
		dev := int(unix.Mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor)))
		if err := unix.Mknodat(parent, base, kind|mode, dev); err != nil {
			return err
		}
	default:
		return nil // PAX global headers and other metadata-only entries
	}

	if err := unix.Fchownat(parent, base, hdr.Uid, hdr.Gid, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("chown: %w", err)
	}
	if hdr.Typeflag != tar.TypeSymlink {
		// chmod after chown, because chown clears setuid and setgid bits.
		if err := unix.Fchmodat(parent, base, mode, 0); err != nil {
			return fmt.Errorf("chmod: %w", err)
		}
	}
	for key, value := range hdr.PAXRecords {
		attr, ok := strings.CutPrefix(key, "SCHILY.xattr.")
		// trusted.overlay.* attributes steer overlayfs itself, so a layer
		// must never be able to set them.
		if !ok || strings.HasPrefix(attr, "trusted.") {
			continue
		}
		err := unix.Lsetxattr(fsutil.ProcPath(parent)+"/"+base, attr, []byte(value), 0)
		if err != nil && !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EPERM) {
			return fmt.Errorf("xattr %s: %w", attr, err)
		}
	}
	if hdr.Typeflag != tar.TypeDir {
		atime := hdr.AccessTime
		if atime.IsZero() {
			atime = hdr.ModTime
		}
		if ts, ok := timespecs(atime, hdr.ModTime); ok {
			unix.UtimesNanoAt(parent, base, ts, unix.AT_SYMLINK_NOFOLLOW)
		}
	}
	return nil
}

// removeEntry deletes name from parent if present, so a later tar entry
// can replace an earlier one of a different type.
func removeEntry(parent int, name string) error {
	err := unix.Unlinkat(parent, name, 0)
	switch {
	case err == nil, errors.Is(err, unix.ENOENT):
		return nil
	case errors.Is(err, unix.EISDIR):
		return os.RemoveAll(fsutil.ProcPath(parent) + "/" + name)
	}
	return err
}

func timespecs(atime, mtime time.Time) ([]unix.Timespec, bool) {
	if mtime.IsZero() {
		return nil, false
	}
	return []unix.Timespec{
		unix.NsecToTimespec(atime.UnixNano()),
		unix.NsecToTimespec(mtime.UnixNano()),
	}, true
}
