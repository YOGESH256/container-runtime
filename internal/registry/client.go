// Package registry is a small client for the OCI Distribution API, the
// HTTP protocol every container registry speaks.
//
// Pulling an image takes three kinds of request:
//
//	GET /v2/<repo>/manifests/<tag or digest>   which blobs make up the image
//	GET /v2/<repo>/blobs/<digest>              config and layer contents
//	GET <realm>?service=..&scope=..            a bearer token, when asked
//
// Registries such as Docker Hub answer the first request with 401 and a
// WWW-Authenticate header naming a token server. The client fetches an
// anonymous token there and retries.
package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const maxManifestSize = 4 << 20

// Client talks to registries. It is safe for concurrent use.
type Client struct {
	HTTP *http.Client

	// Mirror, when set, serves docker.io content instead of Docker Hub,
	// like dockerd's registry-mirrors setting. Example: "mirror.gcr.io".
	Mirror string

	// Username and Password, if set, are sent to token servers.
	Username, Password string

	mu     sync.Mutex
	tokens map[string]string // "registry/repo" -> bearer token
}

// NewFromEnv returns a client configured from MYCRT_REGISTRY_MIRROR and
// MYCRT_REGISTRY_AUTH ("user:password").
func NewFromEnv() *Client {
	c := &Client{
		HTTP: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			MaxIdleConnsPerHost:   8,
		}},
		Mirror: os.Getenv("MYCRT_REGISTRY_MIRROR"),
	}
	if auth := os.Getenv("MYCRT_REGISTRY_AUTH"); auth != "" {
		c.Username, c.Password, _ = strings.Cut(auth, ":")
	}
	return c
}

// Error is a failed registry response.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

// IsNotFound reports whether err is a registry 404.
func IsNotFound(err error) bool {
	var re *Error
	return errors.As(err, &re) && re.Status == http.StatusNotFound
}

// Manifest fetches a manifest or index by tag or digest. It returns the
// raw bytes, their media type and their sha256 digest. When ref is a
// digest, the bytes are checked against it.
func (c *Client) Manifest(ctx context.Context, registry, repo, ref string, accept []string) ([]byte, string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(registry, repo, "manifests", ref), nil)
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("Accept", strings.Join(accept, ", "))
	resp, err := c.do(req, registry, repo)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", responseError(resp, repo+":"+ref)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestSize+1))
	if err != nil {
		return nil, "", "", err
	}
	if len(body) > maxManifestSize {
		return nil, "", "", fmt.Errorf("manifest for %s is larger than %d bytes", ref, maxManifestSize)
	}
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if strings.HasPrefix(ref, "sha256:") && ref != digest {
		return nil, "", "", fmt.Errorf("manifest digest mismatch: asked for %s, content hashes to %s", ref, digest)
	}
	if hdr := resp.Header.Get("Docker-Content-Digest"); hdr != "" && hdr != digest {
		return nil, "", "", fmt.Errorf("manifest digest mismatch: registry says %s, content hashes to %s", hdr, digest)
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return body, mediaType, digest, nil
}

// Resolve asks for a manifest's digest with a HEAD request, without
// downloading it. Docker Hub does not count HEAD requests against its
// pull rate limit, so this is how an up-to-date image is detected cheaply.
func (c *Client) Resolve(ctx context.Context, registry, repo, ref string, accept []string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.url(registry, repo, "manifests", ref), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", strings.Join(accept, ", "))
	resp, err := c.do(req, registry, repo)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", responseError(resp, repo+":"+ref)
	}
	return resp.Header.Get("Docker-Content-Digest"), nil
}

// Blob opens a blob for streaming. The caller must verify its digest,
// since the bytes may come from a CDN the registry redirected to.
func (c *Client) Blob(ctx context.Context, registry, repo, digest string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(registry, repo, "blobs", digest), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req, registry, repo)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, responseError(resp, repo+"@"+digest)
	}
	return resp.Body, nil
}

// Endpoint is the base URL for a registry name.
func (c *Client) Endpoint(registry string) string {
	host := registry
	if registry == "docker.io" {
		host = "registry-1.docker.io"
		if c.Mirror != "" {
			host = c.Mirror
		}
	}
	if strings.Contains(host, "://") {
		return strings.TrimSuffix(host, "/")
	}
	if strings.HasPrefix(host, "localhost") || strings.HasPrefix(host, "127.0.0.1") {
		return "http://" + host
	}
	return "https://" + host
}

func (c *Client) url(registry, repo, kind, ref string) string {
	return fmt.Sprintf("%s/v2/%s/%s/%s", c.Endpoint(registry), repo, kind, ref)
}

// do sends req with a cached token if there is one. On 401 it follows the
// WWW-Authenticate challenge to get a token and retries once.
func (c *Client) do(req *http.Request, registry, repo string) (*http.Response, error) {
	key := registry + "/" + repo
	c.mu.Lock()
	token := c.tokens[key]
	c.mu.Unlock()
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	resp.Body.Close()
	token, err = c.fetchToken(req.Context(), challenge, repo)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.tokens == nil {
		c.tokens = map[string]string{}
	}
	c.tokens[key] = token
	c.mu.Unlock()
	retry := req.Clone(req.Context())
	retry.Header.Set("Authorization", "Bearer "+token)
	return c.HTTP.Do(retry)
}

func (c *Client) fetchToken(ctx context.Context, challenge, repo string) (string, error) {
	scheme, params := ParseChallenge(challenge)
	if !strings.EqualFold(scheme, "bearer") || params["realm"] == "" {
		return "", fmt.Errorf("registry wants %q authentication, which mycrt does not support", scheme)
	}
	q := url.Values{}
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + repo + ":pull"
	}
	q.Set("scope", scope)
	sep := "?"
	if strings.Contains(params["realm"], "?") {
		sep = "&"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, params["realm"]+sep+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	if c.Username != "" {
		req.SetBasicAuth(c.Username, c.Password)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", responseError(resp, "token for "+repo)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("token response: %w", err)
	}
	if body.Token != "" {
		return body.Token, nil
	}
	if body.AccessToken != "" {
		return body.AccessToken, nil
	}
	return "", errors.New("token server returned no token")
}

// ParseChallenge splits a header like
//
//	Bearer realm="https://auth.docker.io/token",service="registry.docker.io"
//
// into its scheme and parameters. Quoted values may contain commas.
func ParseChallenge(h string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	params := map[string]string{}
	for rest = strings.TrimSpace(rest); rest != ""; {
		key, after, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		var val string
		if strings.HasPrefix(after, `"`) {
			end := strings.Index(after[1:], `"`)
			if end < 0 {
				val, rest = after[1:], ""
			} else {
				val, rest = after[1:1+end], after[2+end:]
			}
		} else {
			val, rest, _ = strings.Cut(after, ",")
			rest = "," + rest
		}
		params[key] = val
		rest = strings.TrimLeft(rest, ", ")
	}
	return scheme, params
}

// responseError turns a non-2xx response into a readable error, using the
// registry's JSON error body when there is one.
func responseError(resp *http.Response, what string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var parsed struct {
		Errors []struct{ Code, Message string } `json:"errors"`
	}
	detail := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &parsed) == nil && len(parsed.Errors) > 0 {
		detail = parsed.Errors[0].Code + ": " + parsed.Errors[0].Message
	}
	msg := fmt.Sprintf("%s: HTTP %d", what, resp.StatusCode)
	switch resp.StatusCode {
	case http.StatusNotFound:
		msg = what + ": not found"
	case http.StatusTooManyRequests:
		msg = what + ": registry rate limit reached (HTTP 429). Docker Hub limits anonymous pulls per hour; " +
			"wait, or set MYCRT_REGISTRY_MIRROR=mirror.gcr.io"
	}
	if detail != "" && len(detail) < 300 {
		msg += " (" + detail + ")"
	}
	return &Error{Status: resp.StatusCode, Message: msg}
}
