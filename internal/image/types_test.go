package image

import "testing"

func TestSelectPlatform(t *testing.T) {
	idx := &Index{Manifests: []Descriptor{
		{Digest: "amd64", Platform: &Platform{OS: "linux", Architecture: "amd64"}},
		{Digest: "armv7", Platform: &Platform{OS: "linux", Architecture: "arm", Variant: "v7"}},
		{Digest: "arm64-any", Platform: &Platform{OS: "linux", Architecture: "arm64"}},
		{Digest: "arm64v8", Platform: &Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}},
		{Digest: "attest", Platform: &Platform{OS: "unknown", Architecture: "unknown"}},
	}}
	for _, c := range []struct{ arch, variant, want string }{
		{"arm64", "v8", "arm64v8"}, {"amd64", "", "amd64"}, {"arm", "v7", "armv7"},
	} {
		d, err := SelectPlatform(idx, "linux", c.arch, c.variant)
		if err != nil || d.Digest != c.want {
			t.Errorf("%s/%s: got %q, %v; want %q", c.arch, c.variant, d.Digest, err, c.want)
		}
	}
	if _, err := SelectPlatform(idx, "linux", "riscv64", ""); err == nil {
		t.Error("riscv64: want error")
	}
	// Without an exact variant match, an entry with no variant is preferred.
	d, _ := SelectPlatform(idx, "linux", "arm64", "v9")
	if d.Digest != "arm64-any" {
		t.Errorf("arm64/v9: got %q", d.Digest)
	}
}

func TestIsIndex(t *testing.T) {
	if !IsIndex(MediaTypeDockerList, nil) || IsIndex(MediaTypeDockerManifest, nil) {
		t.Error("media type not honored")
	}
	if !IsIndex("", []byte(`{"manifests":[]}`)) || IsIndex("", []byte(`{"config":{}}`)) {
		t.Error("body sniffing wrong")
	}
}
