package image

import "testing"

func TestParseReference(t *testing.T) {
	const d = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct{ in, registry, repo, tag, digest, str string }{
		{"alpine", "docker.io", "library/alpine", "latest", "", "docker.io/library/alpine:latest"},
		{"alpine:3.20", "docker.io", "library/alpine", "3.20", "", "docker.io/library/alpine:3.20"},
		{"bitnami/redis:7", "docker.io", "bitnami/redis", "7", "", "docker.io/bitnami/redis:7"},
		{"docker.io/library/nginx", "docker.io", "library/nginx", "latest", "", "docker.io/library/nginx:latest"},
		{"index.docker.io/nginx:1", "docker.io", "library/nginx", "1", "", "docker.io/library/nginx:1"},
		{"ghcr.io/org/team/app:v1.2", "ghcr.io", "org/team/app", "v1.2", "", "ghcr.io/org/team/app:v1.2"},
		{"localhost:5000/app", "localhost:5000", "app", "latest", "", "localhost:5000/app:latest"},
		{"localhost/app:dev", "localhost", "app", "dev", "", "localhost/app:dev"},
		{"nginx@" + d, "docker.io", "library/nginx", "", d, "docker.io/library/nginx@" + d},
		{"nginx:1.27@" + d, "docker.io", "library/nginx", "1.27", d, "docker.io/library/nginx:1.27@" + d},
		{"my-org/my_app__x:tag", "docker.io", "my-org/my_app__x", "tag", "", "docker.io/my-org/my_app__x:tag"},
	}
	for _, c := range cases {
		r, err := ParseReference(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if r.Registry != c.registry || r.Repository != c.repo || r.Tag != c.tag || r.Digest != c.digest || r.String() != c.str {
			t.Errorf("%q: got %+v (%s)", c.in, r, r)
		}
	}
	for _, in := range []string{"", "Alpine", "alpine:", "alpine@sha256:xyz", "a//b", "-a", "alpine:bad tag", "ghcr.io/"} {
		if r, err := ParseReference(in); err == nil {
			t.Errorf("%q: parsed as %+v, want error", in, r)
		}
	}
}

func TestShortAndRef(t *testing.T) {
	r, _ := ParseReference("alpine:3.20")
	if r.Short() != "alpine:3.20" || r.Ref() != "3.20" {
		t.Errorf("got %q %q", r.Short(), r.Ref())
	}
	r, _ = ParseReference("bitnami/redis")
	if r.Short() != "bitnami/redis:latest" {
		t.Errorf("got %q", r.Short())
	}
}
