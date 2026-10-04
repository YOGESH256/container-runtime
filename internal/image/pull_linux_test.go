//go:build linux

package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YOGESH256/container-runtime/internal/registry"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"
)

type entry struct {
	name, body, link string
	typ              byte
	mode             int64
}

// layer builds a gzipped tar and returns it with its diff ID.
func layer(t *testing.T, entries ...entry) ([]byte, string) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Linkname: e.link, Mode: e.mode, Size: int64(len(e.body))}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		io.WriteString(tw, e.body)
	}
	tw.Close()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(raw.Bytes())
	zw.Close()
	return gz.Bytes(), sha256Digest(raw.Bytes())
}

// fakeImage serves one image at 127.0.0.1:<port>/test/img:v1.
type fakeImage struct {
	blobs    map[string][]byte
	manifest []byte
	ref      Reference
	srv      *httptest.Server
}

func newFakeImage(t *testing.T, layers [][]byte, diffIDs []string) *fakeImage {
	f := &fakeImage{blobs: map[string][]byte{}}
	var cfg Config
	cfg.OS, cfg.Architecture = "linux", "arm64"
	cfg.Config.Cmd = []string{"/bin/true"}
	cfg.RootFS.Type, cfg.RootFS.DiffIDs = "layers", diffIDs
	cfgBytes, _ := json.Marshal(cfg)
	m := Manifest{SchemaVersion: 2, MediaType: MediaTypeOCIManifest,
		Config: Descriptor{MediaType: MediaTypeOCIConfig, Digest: sha256Digest(cfgBytes), Size: int64(len(cfgBytes))}}
	f.blobs[m.Config.Digest] = cfgBytes
	for _, l := range layers {
		d := Descriptor{MediaType: MediaTypeOCILayerGzip, Digest: sha256Digest(l), Size: int64(len(l))}
		m.Layers = append(m.Layers, d)
		f.blobs[d.Digest] = l
	}
	f.manifest, _ = json.Marshal(m)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/test/img/manifests/v1":
			w.Header().Set("Content-Type", MediaTypeOCIManifest)
			w.Write(f.manifest)
		case strings.HasPrefix(r.URL.Path, "/v2/test/img/blobs/"):
			b, ok := f.blobs[strings.TrimPrefix(r.URL.Path, "/v2/test/img/blobs/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Write(b)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	f.ref = Reference{Registry: strings.TrimPrefix(f.srv.URL, "http://"), Repository: "test/img", Tag: "v1"}
	return f
}

func (f *fakeImage) pull(t *testing.T, s *Store) (*Image, error) {
	return s.Pull(context.Background(), &registry.Client{HTTP: f.srv.Client()}, f.ref, io.Discard)
}

func requireRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: creates device nodes, sets trusted xattrs and mounts overlayfs")
	}
}

func TestPullUnpacksAndConvertsWhiteouts(t *testing.T) {
	requireRoot(t)
	outside := t.TempDir() // must stay empty
	base, baseID := layer(t,
		entry{name: "etc/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "etc/hello", typ: tar.TypeReg, body: "hi"},
		entry{name: "var/old", typ: tar.TypeReg, body: "old"},
		entry{name: "bin/tool", typ: tar.TypeReg, body: "#!", mode: 0o4755},
		entry{name: "bin/tool-link", typ: tar.TypeLink, link: "bin/tool"},
		// Attempts to write outside the layer directory:
		entry{name: "../../escape-dotdot", typ: tar.TypeReg, body: "x"},
		entry{name: "hostlink", typ: tar.TypeSymlink, link: outside},
		entry{name: "hostlink/escape-symlink", typ: tar.TypeReg, body: "x"},
	)
	top, topID := layer(t,
		entry{name: "etc/.wh.hello", typ: tar.TypeReg},
		entry{name: "var/.wh..wh..opq", typ: tar.TypeReg},
		entry{name: "var/new", typ: tar.TypeReg, body: "new"},
	)
	f := newFakeImage(t, [][]byte{base, top}, []string{baseID, topID})
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	img, err := f.pull(t, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(img.LayerDirs) != 2 {
		t.Fatalf("layer dirs = %v", img.LayerDirs)
	}
	low, up := img.LayerDirs[0], img.LayerDirs[1]

	if b, _ := os.ReadFile(filepath.Join(low, "etc/hello")); string(b) != "hi" {
		t.Errorf("etc/hello = %q", b)
	}
	var st unix.Stat_t
	unix.Stat(filepath.Join(low, "bin/tool"), &st)
	if st.Mode&0o7777 != 0o4755 || st.Nlink != 2 {
		t.Errorf("bin/tool mode %o nlink %d, want 4755 and a hard link", st.Mode&0o7777, st.Nlink)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("a symlink let a tar entry escape the layer: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(low, "escape-dotdot")); err != nil {
		t.Errorf("../../escape-dotdot should be clamped inside the layer: %v", err)
	}
	if _, err := os.Stat(filepath.Join(low, outside, "escape-symlink")); err != nil {
		t.Errorf("hostlink/escape-symlink should resolve inside the layer: %v", err)
	}

	unix.Lstat(filepath.Join(up, "etc/hello"), &st)
	if st.Mode&unix.S_IFMT != unix.S_IFCHR || st.Rdev != 0 {
		t.Errorf("whiteout for etc/hello is mode %o rdev %d, want a 0:0 char device", st.Mode, st.Rdev)
	}
	if v, err := unix.Getxattr(filepath.Join(up, "var"), "trusted.overlay.opaque", make([]byte, 8)); err != nil || v != 1 {
		t.Errorf("var is not marked opaque: %v", err)
	}

	// Ask the kernel: stack the layers and look at the merged view.
	merged, upper, work := t.TempDir(), t.TempDir(), t.TempDir()
	opts := "lowerdir=" + up + ":" + low + ",upperdir=" + upper + ",workdir=" + work
	if err := unix.Mount("overlay", merged, "overlay", 0, opts); err != nil {
		t.Fatal(err)
	}
	defer unix.Unmount(merged, unix.MNT_DETACH)
	if _, err := os.Stat(filepath.Join(merged, "etc/hello")); !os.IsNotExist(err) {
		t.Errorf("etc/hello visible through overlay: %v", err)
	}
	if names, _ := os.ReadDir(filepath.Join(merged, "var")); len(names) != 1 || names[0].Name() != "new" {
		t.Errorf("merged var/ = %v, want only new", names)
	}
}

func TestPullRejectsCorruptLayer(t *testing.T) {
	requireRoot(t)
	l, id := layer(t, entry{name: "f", typ: tar.TypeReg, body: "content"})
	f := newFakeImage(t, [][]byte{l}, []string{id})
	// Flip one byte of the served blob but keep its advertised digest.
	d := sha256Digest(l)
	bad := bytes.Clone(l)
	bad[len(bad)-5] ^= 0xff
	f.blobs[d] = bad
	s, _ := NewStore(t.TempDir())
	_, err := f.pull(t, s)
	if err == nil || !(strings.Contains(err.Error(), "digest mismatch") || strings.Contains(err.Error(), "gzip") || strings.Contains(err.Error(), "checksum")) {
		t.Fatalf("err = %v, want a verification failure", err)
	}
	if _, err := os.Stat(s.LayerDir(id)); !os.IsNotExist(err) {
		t.Error("corrupt layer was committed to the store")
	}
	if left, _ := os.ReadDir(s.path("tmp")); len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
}

func TestPullZstdLayer(t *testing.T) {
	requireRoot(t)
	raw, _ := layer(t, entry{name: "hello", typ: tar.TypeReg, body: "zstd"})
	// layer() gzips; re-encode the tar with zstd instead.
	gz, _ := gzip.NewReader(bytes.NewReader(raw))
	tarBytes, _ := io.ReadAll(gz)
	var zbuf bytes.Buffer
	zw, _ := zstd.NewWriter(&zbuf)
	zw.Write(tarBytes)
	zw.Close()
	f := newFakeImage(t, [][]byte{zbuf.Bytes()}, []string{sha256Digest(tarBytes)})
	var m Manifest
	json.Unmarshal(f.manifest, &m)
	m.Layers[0].MediaType = MediaTypeOCILayerZstd
	f.manifest, _ = json.Marshal(m)
	s, _ := NewStore(t.TempDir())
	img, err := f.pull(t, s)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(img.LayerDirs[0], "hello")); string(b) != "zstd" {
		t.Fatalf("hello = %q", b)
	}
}

func TestPullRejectsWrongDiffID(t *testing.T) {
	requireRoot(t)
	l, _ := layer(t, entry{name: "f", typ: tar.TypeReg, body: "content"})
	wrong := "sha256:" + strings.Repeat("ab", 32)
	f := newFakeImage(t, [][]byte{l}, []string{wrong})
	s, _ := NewStore(t.TempDir())
	if _, err := f.pull(t, s); err == nil || !strings.Contains(err.Error(), "diff ID mismatch") {
		t.Fatalf("err = %v, want diff ID mismatch", err)
	}
}
