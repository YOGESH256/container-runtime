// Package paths names the directories where mycrt keeps its data.
package paths

import (
	"os"
	"path/filepath"
)

// Root holds everything that must survive a reboot: pulled images and each
// container's writable layer.
var Root = "/var/lib/mycrt"

// Containers holds one directory per container.
func Containers() string { return filepath.Join(Root, "containers") }

// Container is one container's directory. Inside it:
//
//	upper/   the container's writable overlay layer
//	work/    overlayfs scratch space, same filesystem as upper
//	rootfs/  mount point where the merged view is assembled
func Container(id string) string { return filepath.Join(Containers(), id) }

// Images holds pulled images:
//
//	blobs/sha256/<hex>   manifests, indexes and config blobs
//	layers/<hex>/        each layer unpacked on its own, named by its diff ID
//	refs.json            image names mapped to the manifests they resolved to
func Images() string { return filepath.Join(Root, "images") }

// Network holds IP leases: leases.json maps each address to its container.
func Network() string { return filepath.Join(Root, "network") }

// Ensure creates the data directories. The ones on the way to a
// container's layers are traversable (0711) but not listable, because the
// container's unprivileged user must reach them.
func Ensure() error {
	for _, d := range []string{Root, Containers(), Images()} {
		if err := os.MkdirAll(d, 0o711); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o711); err != nil {
			return err
		}
	}
	return os.MkdirAll(Network(), 0o700)
}
