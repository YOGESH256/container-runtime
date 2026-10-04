package image

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Media types from the OCI image spec and Docker's older schema 2, which
// registries still serve for many images.
const (
	MediaTypeOCIIndex       = "application/vnd.oci.image.index.v1+json"
	MediaTypeDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"

	MediaTypeOCIConfig    = "application/vnd.oci.image.config.v1+json"
	MediaTypeDockerConfig = "application/vnd.docker.container.image.v1+json"

	MediaTypeOCILayer        = "application/vnd.oci.image.layer.v1.tar"
	MediaTypeOCILayerGzip    = "application/vnd.oci.image.layer.v1.tar+gzip"
	MediaTypeOCILayerZstd    = "application/vnd.oci.image.layer.v1.tar+zstd"
	MediaTypeDockerLayerGzip = "application/vnd.docker.image.rootfs.diff.tar.gzip"
)

// ManifestMediaTypes is the Accept list sent when fetching a manifest.
var ManifestMediaTypes = []string{
	MediaTypeOCIIndex, MediaTypeDockerList, MediaTypeOCIManifest, MediaTypeDockerManifest,
}

// Descriptor points at a piece of content by digest.
type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Platform    *Platform         `json:"platform,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Platform says which OS and CPU an image variant is built for.
type Platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
}

func (p Platform) String() string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// Index lists one manifest per platform for a multi-arch image.
type Index struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType,omitempty"`
	Manifests     []Descriptor `json:"manifests"`
}

// Manifest describes one platform's image: a config blob and its layers,
// listed from the bottom layer up.
type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType,omitempty"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
}

// Config is the image configuration blob.
type Config struct {
	Architecture string          `json:"architecture"`
	OS           string          `json:"os"`
	Variant      string          `json:"variant,omitempty"`
	Config       ContainerConfig `json:"config"`
	RootFS       struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"` // digests of the uncompressed layers
	} `json:"rootfs"`
}

// ContainerConfig holds the defaults for running the image.
type ContainerConfig struct {
	User         string              `json:"User,omitempty"`
	Env          []string            `json:"Env,omitempty"`
	Entrypoint   []string            `json:"Entrypoint,omitempty"`
	Cmd          []string            `json:"Cmd,omitempty"`
	WorkingDir   string              `json:"WorkingDir,omitempty"`
	StopSignal   string              `json:"StopSignal,omitempty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	Labels       map[string]string   `json:"Labels,omitempty"`
}

// IsIndex reports whether a manifest response is a multi-platform index.
// Some registries omit the Content-Type, so the body's shape decides then.
func IsIndex(mediaType string, body []byte) bool {
	switch mediaType {
	case MediaTypeOCIIndex, MediaTypeDockerList:
		return true
	case MediaTypeOCIManifest, MediaTypeDockerManifest:
		return false
	}
	var probe struct {
		Manifests json.RawMessage `json:"manifests"`
	}
	return json.Unmarshal(body, &probe) == nil && probe.Manifests != nil
}

// SelectPlatform picks the manifest for os/arch from an index. An exact
// variant match wins, then an entry with no variant, then any variant.
func SelectPlatform(idx *Index, os, arch, variant string) (Descriptor, error) {
	best, bestScore := Descriptor{}, 0
	var available []string
	for _, d := range idx.Manifests {
		p := d.Platform
		if p == nil || p.OS == "unknown" { // e.g. build attestations
			continue
		}
		available = append(available, p.String())
		if p.OS != os || p.Architecture != arch {
			continue
		}
		score := 1
		switch p.Variant {
		case variant:
			score = 3
		case "":
			score = 2
		}
		if score > bestScore {
			best, bestScore = d, score
		}
	}
	if bestScore == 0 {
		sort.Strings(available)
		return best, fmt.Errorf("image has no %s/%s variant (available: %s)", os, arch, strings.Join(available, ", "))
	}
	return best, nil
}

// Compression is how a layer blob is encoded.
type Compression int

const (
	Uncompressed Compression = iota
	Gzip
	Zstd
)

// LayerCompression says how to decode a layer blob.
func LayerCompression(mediaType string) (Compression, error) {
	switch mediaType {
	case MediaTypeOCILayerGzip, MediaTypeDockerLayerGzip:
		return Gzip, nil
	case MediaTypeOCILayerZstd:
		return Zstd, nil
	case MediaTypeOCILayer:
		return Uncompressed, nil
	}
	return 0, fmt.Errorf("unsupported layer media type %q", mediaType)
}
