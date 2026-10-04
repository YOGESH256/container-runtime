//go:build linux

package image

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/YOGESH256/container-runtime/internal/fsync"
	"github.com/YOGESH256/container-runtime/internal/registry"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"
)

const (
	maxConfigSize     = 32 << 20
	parallelDownloads = 3
	platformOS        = "linux"
)

// Pull fetches ref from its registry into the store and returns it.
//
// Steps: resolve the name to a manifest (following a multi-arch index to
// this machine's platform), fetch the config, then download and unpack
// every layer not already present. Nothing is trusted on the way: each
// blob is hashed and compared with the digest that referenced it.
func (s *Store) Pull(ctx context.Context, c *registry.Client, ref Reference, log io.Writer) (*Image, error) {
	fmt.Fprintf(log, "Pulling %s\n", ref.Short())
	start := time.Now()

	// A HEAD request is enough to see that nothing changed.
	if refs, err := s.readRefs(); err == nil {
		if rec, ok := refs[ref.String()]; ok {
			if d, err := c.Resolve(ctx, ref.Registry, ref.Repository, ref.Ref(), ManifestMediaTypes); err == nil && d == rec.Digest {
				if img, err := s.load(ref.String(), rec); err == nil {
					fmt.Fprintf(log, "  up to date (%s)\n", shortDigest(rec.Digest))
					return img, nil
				}
			}
		}
	}

	body, mediaType, top, err := c.Manifest(ctx, ref.Registry, ref.Repository, ref.Ref(), ManifestMediaTypes)
	if err != nil {
		return nil, err
	}
	if err := s.writeBlob(top, body); err != nil {
		return nil, err
	}
	manifestDigest := top
	if IsIndex(mediaType, body) {
		var idx Index
		if err := json.Unmarshal(body, &idx); err != nil {
			return nil, fmt.Errorf("parse index: %w", err)
		}
		desc, err := SelectPlatform(&idx, platformOS, runtime.GOARCH, archVariant())
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(log, "  index %s: using %s manifest %s\n", shortDigest(top), desc.Platform, shortDigest(desc.Digest))
		body, mediaType, manifestDigest, err = c.Manifest(ctx, ref.Registry, ref.Repository, desc.Digest, ManifestMediaTypes)
		if err != nil {
			return nil, err
		}
		if IsIndex(mediaType, body) {
			return nil, fmt.Errorf("nested image indexes are not supported")
		}
		if err := s.writeBlob(manifestDigest, body); err != nil {
			return nil, err
		}
	}

	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.SchemaVersion != 2 || m.Config.Digest == "" {
		return nil, fmt.Errorf("unsupported manifest (schema version %d)", m.SchemaVersion)
	}
	cfgBytes, err := s.fetchBlob(ctx, c, ref, m.Config)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(cfgBytes, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.OS != "" && cfg.OS != platformOS {
		return nil, fmt.Errorf("image is for %s, not %s", cfg.OS, platformOS)
	}
	if len(cfg.RootFS.DiffIDs) != len(m.Layers) {
		return nil, fmt.Errorf("manifest lists %d layers but config has %d diff IDs", len(m.Layers), len(cfg.RootFS.DiffIDs))
	}

	if err := s.fetchLayers(ctx, c, ref, m.Layers, cfg.RootFS.DiffIDs, log); err != nil {
		return nil, err
	}
	rec := RefRecord{Digest: top, Manifest: manifestDigest, PulledAt: time.Now().UTC()}
	if err := s.updateRefs(func(refs map[string]RefRecord) { refs[ref.String()] = rec }); err != nil {
		return nil, err
	}
	img, err := s.load(ref.String(), rec)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(log, "  pulled %s: %d layers, %s in %.1fs\n", ref.Short(), len(m.Layers),
		humanSize(img.CompressedSize), time.Since(start).Seconds())
	return img, nil
}

// fetchBlob returns a small blob such as a config, from disk if present.
func (s *Store) fetchBlob(ctx context.Context, c *registry.Client, ref Reference, d Descriptor) ([]byte, error) {
	if b, err := s.readBlob(d.Digest); err == nil {
		return b, nil
	}
	if d.Size > maxConfigSize {
		return nil, fmt.Errorf("blob %s is too large (%d bytes)", d.Digest, d.Size)
	}
	rc, err := c.Blob(ctx, ref.Registry, ref.Repository, d.Digest)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, d.Size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != d.Size {
		return nil, fmt.Errorf("blob %s: got %d bytes, manifest says %d", d.Digest, len(b), d.Size)
	}
	return b, s.writeBlob(d.Digest, b)
}

// fetchLayers downloads missing layers a few at a time.
func (s *Store) fetchLayers(ctx context.Context, c *registry.Client, ref Reference, layers []Descriptor, diffIDs []string, log io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		slots    = make(chan struct{}, parallelDownloads)
	)
	for i := range layers {
		if _, err := os.Stat(s.LayerDir(diffIDs[i])); err == nil {
			fmt.Fprintf(log, "  layer %d/%d %s already present\n", i+1, len(layers), shortDigest(layers[i].Digest))
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			t := time.Now()
			err := s.fetchLayer(ctx, c, ref, layers[i], diffIDs[i])
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("layer %s: %w", shortDigest(layers[i].Digest), err)
					cancel()
				}
				return
			}
			fmt.Fprintf(log, "  layer %d/%d %s %s verified and unpacked in %.1fs\n", i+1, len(layers),
				shortDigest(layers[i].Digest), humanSize(layers[i].Size), time.Since(t).Seconds())
		}(i)
	}
	wg.Wait()
	return firstErr
}

// fetchLayer streams one layer from the registry straight into a fresh
// directory, hashing as it goes:
//
//	HTTP body -> sha256 of compressed bytes  (must equal the manifest digest)
//	          -> gunzip
//	          -> sha256 of the tar stream    (must equal the config's diff ID)
//	          -> Unpack
//
// The directory is renamed into place only if both hashes match.
func (s *Store) fetchLayer(ctx context.Context, c *registry.Client, ref Reference, d Descriptor, diffID string) error {
	compression, err := LayerCompression(d.MediaType)
	if err != nil {
		return err
	}
	body, err := c.Blob(ctx, ref.Registry, ref.Repository, d.Digest)
	if err != nil {
		return err
	}
	defer body.Close()

	tmp, err := os.MkdirTemp(s.path("tmp"), "layer-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	compressed := newHashingReader(io.LimitReader(body, d.Size+1))
	var tarStream io.Reader = compressed
	switch compression {
	case Gzip:
		zr, err := gzip.NewReader(compressed)
		if err != nil {
			return fmt.Errorf("gunzip: %w", err)
		}
		defer zr.Close()
		tarStream = zr
	case Zstd:
		zr, err := zstd.NewReader(compressed)
		if err != nil {
			return fmt.Errorf("zstd: %w", err)
		}
		defer zr.Close()
		tarStream = zr
	}
	uncompressed := newHashingReader(tarStream)
	if err := Unpack(uncompressed, tmp, s.IDShift); err != nil {
		return err
	}
	// The tar reader stops at the end-of-archive marker. Read the padding
	// after it too, so both hashes cover every byte and gzip checks its CRC.
	if _, err := io.Copy(io.Discard, uncompressed); err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, compressed); err != nil {
		return err
	}

	if compressed.n != d.Size {
		return fmt.Errorf("size mismatch: got %d bytes, manifest says %d", compressed.n, d.Size)
	}
	if got := compressed.digest(); got != d.Digest {
		return fmt.Errorf("digest mismatch: got %s, manifest says %s", got, d.Digest)
	}
	if got := uncompressed.digest(); got != diffID {
		return fmt.Errorf("diff ID mismatch: uncompressed layer hashes to %s, config says %s", got, diffID)
	}
	// Flush the unpacked files before the rename makes the layer visible.
	// Otherwise a power loss could leave a committed layer full of empty
	// files. syncfs flushes the whole filesystem, which is fine for a pull.
	if err := syncfs(tmp); err != nil {
		return err
	}
	if err := os.Chown(tmp, s.IDShift, s.IDShift); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.LayerDir(diffID)); err != nil {
		if _, statErr := os.Stat(s.LayerDir(diffID)); statErr == nil {
			return nil // another pull unpacked the same layer first
		}
		return err
	}
	return fsync.Dir(s.layersDir())
}

func syncfs(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return unix.Syncfs(int(f.Fd()))
}

type hashingReader struct {
	r io.Reader
	h hash.Hash
	n int64
}

func newHashingReader(r io.Reader) *hashingReader {
	return &hashingReader{r: r, h: sha256.New()}
}

func (h *hashingReader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	h.h.Write(p[:n])
	h.n += int64(n)
	return n, err
}

func (h *hashingReader) digest() string { return "sha256:" + hex.EncodeToString(h.h.Sum(nil)) }

// archVariant is the CPU variant registries use for this build.
func archVariant() string {
	switch runtime.GOARCH {
	case "arm64":
		return "v8"
	case "arm":
		return "v7"
	}
	return ""
}

func shortDigest(d string) string {
	if len(d) > 19 {
		return d[:19]
	}
	return d
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// IsNotFound reports whether err means the image is not stored locally.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }
