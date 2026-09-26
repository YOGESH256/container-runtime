//go:build linux

package image

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"container-runtime/internal/fsync"

	"golang.org/x/sys/unix"
)

// ErrNotFound means the image has not been pulled.
var ErrNotFound = errors.New("image not found locally")

// Store is a content-addressed image store on local disk. Every file is
// named by the sha256 of its contents, so identical layers shared by
// different images are downloaded and unpacked once.
type Store struct {
	Root string
}

// RefRecord is what a name resolved to when it was pulled.
type RefRecord struct {
	Digest   string    `json:"digest"`   // what the tag pointed to, an index or a manifest
	Manifest string    `json:"manifest"` // the manifest for this machine's platform
	PulledAt time.Time `json:"pulledAt"`
}

// Image is a pulled image, ready to run.
type Image struct {
	Name           string // fully qualified, e.g. docker.io/library/alpine:3.20
	Record         RefRecord
	Manifest       Manifest
	Config         Config
	LayerDirs      []string // bottom layer first
	CompressedSize int64
}

// NewStore creates the store's directories if needed.
func NewStore(root string) (*Store, error) {
	s := &Store{Root: root}
	for _, d := range []string{s.path("blobs", "sha256"), s.path("layers"), s.path("tmp")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) path(elem ...string) string {
	return filepath.Join(append([]string{s.Root}, elem...)...)
}

func (s *Store) blobPath(digest string) string {
	return s.path("blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))
}

// LayerDir is where the layer with this uncompressed digest is unpacked.
func (s *Store) LayerDir(diffID string) string {
	return s.path("layers", strings.TrimPrefix(diffID, "sha256:"))
}

// readBlob returns a stored blob after re-checking its digest.
func (s *Store) readBlob(digest string) ([]byte, error) {
	b, err := os.ReadFile(s.blobPath(digest))
	if err != nil {
		return nil, err
	}
	if got := sha256Digest(b); got != digest {
		return nil, fmt.Errorf("stored blob %s is corrupt (hashes to %s)", digest, got)
	}
	return b, nil
}

func (s *Store) writeBlob(digest string, b []byte) error {
	if got := sha256Digest(b); got != digest {
		return fmt.Errorf("digest mismatch: expected %s, content hashes to %s", digest, got)
	}
	return writeFileAtomic(s.blobPath(digest), b)
}

// Get loads a pulled image by name. It returns ErrNotFound if the name was
// never pulled or its content is incomplete.
func (s *Store) Get(ref Reference) (*Image, error) {
	refs, err := s.readRefs()
	if err != nil {
		return nil, err
	}
	rec, ok := refs[ref.String()]
	if !ok {
		return nil, ErrNotFound
	}
	return s.load(ref.String(), rec)
}

func (s *Store) load(name string, rec RefRecord) (*Image, error) {
	img := &Image{Name: name, Record: rec}
	mb, err := s.readBlob(rec.Manifest)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	if err := json.Unmarshal(mb, &img.Manifest); err != nil {
		return nil, err
	}
	cb, err := s.readBlob(img.Manifest.Config.Digest)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	if err := json.Unmarshal(cb, &img.Config); err != nil {
		return nil, err
	}
	for i, diffID := range img.Config.RootFS.DiffIDs {
		dir := s.LayerDir(diffID)
		if _, err := os.Stat(dir); err != nil {
			return nil, fmt.Errorf("%w: layer %s missing", ErrNotFound, diffID)
		}
		img.LayerDirs = append(img.LayerDirs, dir)
		if i < len(img.Manifest.Layers) {
			img.CompressedSize += img.Manifest.Layers[i].Size
		}
	}
	return img, nil
}

// List returns every pulled image, sorted by name.
func (s *Store) List() ([]*Image, error) {
	refs, err := s.readRefs()
	if err != nil {
		return nil, err
	}
	var out []*Image
	for name, rec := range refs {
		if img, err := s.load(name, rec); err == nil {
			out = append(out, img)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Store) readRefs() (map[string]RefRecord, error) {
	refs := map[string]RefRecord{}
	b, err := os.ReadFile(s.path("refs.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return refs, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &refs); err != nil {
		// Only the name index is lost; content is addressed by digest and
		// re-verified on read. Names resolve again on the next pull.
		fmt.Fprintf(os.Stderr, "mycrt: image index %s is damaged (%v); starting a new one\n", s.path("refs.json"), err)
		return map[string]RefRecord{}, nil
	}
	return refs, nil
}

// updateRefs edits refs.json under an exclusive lock, because several
// mycrt processes may pull at the same time.
func (s *Store) updateRefs(edit func(map[string]RefRecord)) error {
	lock, err := os.OpenFile(s.path("refs.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	refs, err := s.readRefs()
	if err != nil {
		return err
	}
	edit(refs)
	b, err := json.MarshalIndent(refs, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path("refs.json"), b)
}

// writeFileAtomic replaces path durably, so a power loss leaves either
// the old content or the new, never an empty file.
func writeFileAtomic(path string, b []byte) error {
	return fsync.WriteFile(path, b, 0o600)
}

func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
