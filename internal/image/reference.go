// Package image resolves, pulls, verifies and stores OCI images, and
// unpacks their layers in the form overlayfs expects.
package image

import (
	"fmt"
	"regexp"
	"strings"
)

// DockerHub is the registry used when a name has no registry part.
const DockerHub = "docker.io"

var (
	componentRe = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
	tagRe       = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digestRe    = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// Reference is a parsed image name such as "alpine:3.20",
// "ghcr.io/org/app:v1" or "nginx@sha256:...".
type Reference struct {
	Registry   string // "docker.io", "ghcr.io", "localhost:5000"
	Repository string // "library/alpine"
	Tag        string // empty when only a digest was given
	Digest     string // "sha256:<hex>" when pinned
}

// ParseReference applies Docker's naming rules: no registry means Docker
// Hub, a one-part Docker Hub name lives under "library/", and a missing tag
// means "latest".
func ParseReference(s string) (Reference, error) {
	var r Reference
	name := s
	if i := strings.Index(name, "@"); i >= 0 {
		name, r.Digest = name[:i], name[i+1:]
		if !digestRe.MatchString(r.Digest) {
			return r, fmt.Errorf("invalid image %q: digest must be sha256:<64 hex chars>", s)
		}
	}
	// A tag follows the last ':' after the last '/'. A ':' before that
	// belongs to a registry port, as in localhost:5000/app.
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name, r.Tag = name[:i], name[i+1:]
		if !tagRe.MatchString(r.Tag) {
			return r, fmt.Errorf("invalid image %q: bad tag %q", s, r.Tag)
		}
	}
	// The first component is a registry if it looks like a host name.
	r.Registry = DockerHub
	if i := strings.Index(name, "/"); i >= 0 {
		if first := name[:i]; strings.ContainsAny(first, ".:") || first == "localhost" {
			r.Registry, name = first, name[i+1:]
		}
	}
	if r.Registry == "index.docker.io" || r.Registry == "registry-1.docker.io" {
		r.Registry = DockerHub
	}
	if r.Registry == DockerHub && !strings.Contains(name, "/") {
		name = "library/" + name
	}
	if name == "" {
		return r, fmt.Errorf("invalid image %q: empty repository", s)
	}
	for _, c := range strings.Split(name, "/") {
		if !componentRe.MatchString(c) {
			return r, fmt.Errorf("invalid image %q: repository names must be lowercase letters, digits and separators", s)
		}
	}
	r.Repository = name
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}
	return r, nil
}

// String is the fully qualified name, e.g. docker.io/library/alpine:3.20.
func (r Reference) String() string {
	s := r.Registry + "/" + r.Repository
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}

// Short drops the docker.io/library/ prefix people usually leave out.
func (r Reference) Short() string {
	s := r.String()
	s = strings.TrimPrefix(s, DockerHub+"/library/")
	return strings.TrimPrefix(s, DockerHub+"/")
}

// Ref is what to ask the registry for: the digest if pinned, else the tag.
func (r Reference) Ref() string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}
