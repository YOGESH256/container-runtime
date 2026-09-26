package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// fakeRegistry demands a bearer token from its own token endpoint, like
// Docker Hub does, and serves one manifest and one blob.
func fakeRegistry(t *testing.T, manifest, blob []byte) (*httptest.Server, *int32) {
	var tokenRequests int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			atomic.AddInt32(&tokenRequests, 1)
			if r.URL.Query().Get("scope") != "repository:library/demo:pull" || r.URL.Query().Get("service") != "fake" {
				http.Error(w, "bad scope "+r.URL.RawQuery, http.StatusBadRequest)
				return
			}
			io.WriteString(w, `{"token":"secret-token"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="fake",scope="repository:library/demo:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v2/library/demo/manifests/v1", "/v2/library/demo/manifests/" + digestOf(manifest):
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json; charset=utf-8")
			w.Header().Set("Docker-Content-Digest", digestOf(manifest))
			if r.Method == http.MethodGet {
				w.Write(manifest)
			}
		case "/v2/library/demo/blobs/" + digestOf(blob):
			w.Write(blob)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &tokenRequests
}

func registryName(srv *httptest.Server) string { return strings.TrimPrefix(srv.URL, "http://") }

func TestManifestWithTokenAuth(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2,"config":{},"layers":[]}`)
	srv, tokens := fakeRegistry(t, manifest, []byte("blob"))
	c := &Client{HTTP: srv.Client()}
	ctx := context.Background()

	body, mt, dg, err := c.Manifest(ctx, registryName(srv), "library/demo", "v1", []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(manifest) || mt != "application/vnd.oci.image.manifest.v1+json" || dg != digestOf(manifest) {
		t.Fatalf("got %q %q %q", body, mt, dg)
	}
	// The token is cached: a second request needs no new token.
	if _, err := c.Resolve(ctx, registryName(srv), "library/demo", "v1", []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(tokens); n != 1 {
		t.Errorf("token requests = %d, want 1", n)
	}
	rc, err := c.Blob(ctx, registryName(srv), "library/demo", digestOf([]byte("blob")))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "blob" {
		t.Errorf("blob = %q", b)
	}
}

func TestManifestDigestMismatch(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2}`)
	srv, _ := fakeRegistry(t, manifest, nil)
	c := &Client{HTTP: srv.Client()}
	// Serve the right bytes under a digest they don't hash to.
	wrong := "sha256:" + strings.Repeat("0", 64)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(manifest) })
	_, _, _, err := c.Manifest(context.Background(), registryName(srv), "library/demo", wrong, []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("err = %v, want digest mismatch", err)
	}
}

func TestNotFoundAndRateLimit(t *testing.T) {
	srv, _ := fakeRegistry(t, []byte("{}"), nil)
	c := &Client{HTTP: srv.Client()}
	_, _, _, err := c.Manifest(context.Background(), registryName(srv), "library/demo", "nope", []string{"x"})
	if !IsNotFound(err) || !strings.Contains(err.Error(), "MANIFEST_UNKNOWN") {
		t.Fatalf("err = %v, want not found with registry detail", err)
	}
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) })
	_, _, _, err = c.Manifest(context.Background(), registryName(srv), "library/demo", "v1", []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("err = %v, want rate limit message", err)
	}
}

func TestParseChallenge(t *testing.T) {
	scheme, p := ParseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:a/b:pull,push"`)
	if scheme != "Bearer" || p["realm"] != "https://auth.docker.io/token" || p["service"] != "registry.docker.io" || p["scope"] != "repository:a/b:pull,push" {
		t.Fatalf("got %q %v", scheme, p)
	}
	scheme, p = ParseChallenge(`Basic realm=registry`)
	if scheme != "Basic" || p["realm"] != "registry" {
		t.Fatalf("got %q %v", scheme, p)
	}
}

func TestEndpoint(t *testing.T) {
	c := &Client{}
	for in, want := range map[string]string{
		"docker.io": "https://registry-1.docker.io", "ghcr.io": "https://ghcr.io", "localhost:5000": "http://localhost:5000",
	} {
		if got := c.Endpoint(in); got != want {
			t.Errorf("Endpoint(%q) = %q, want %q", in, got, want)
		}
	}
	c.Mirror = "mirror.gcr.io"
	if got := c.Endpoint("docker.io"); got != "https://mirror.gcr.io" {
		t.Errorf("mirror: got %q", got)
	}
}
