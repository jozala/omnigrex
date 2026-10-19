//go:build microsandbox && cgo

package microsandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/distribution/reference"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	digest "github.com/opencontainers/go-digest"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sys/unix"
)

const imageMetadataLimit = 4 << 20

var errImageUnavailable = errors.New("Microsandbox immutable image verification unavailable")
var errImageCorrupt = errors.New("Microsandbox immutable image metadata corrupt")

// SDK 0.7.6 GlobalCache stores the selected manifest and raw config in
// cache/manifests/<sha256(normalized-reference)>.json. Image.Get exposes only
// database metadata, and Image.Load permits arbitrary digest-shaped aliases.
// Neither of those claims alone establishes an immutable image identity.
// Original registry manifests and parent indexes are served from a persistent
// content-addressed document cache below the configured home so repeated
// readiness checks reuse verified immutable bytes without registry traffic.
func verifyCachedImage(ctx context.Context, home, ref, manifestDigest, imageOS, arch string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return errImageUnavailable
	}
	defer root.Close()
	key := sha256.Sum256([]byte(ref)) // Exact references are already normalized.
	file, err := root.OpenFile(filepath.Join("cache", "manifests", hex.EncodeToString(key[:])+".json"), os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return errImageUnavailable
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > imageMetadataLimit {
		return errImageCorrupt
	}
	content, err := readImageDocument(file, imageMetadataLimit)
	if err != nil {
		return err
	}
	cache := newRegistryDocumentCache(home, nil)
	return verifyImageContent(ctx, ref, manifestDigest, imageOS, arch, content, cache.fetch)
}

func readImageDocument(reader io.Reader, limit int64) ([]byte, error) {
	content, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil || int64(len(content)) > limit {
		return nil, errImageCorrupt
	}
	return content, nil
}

func verifyImageContent(ctx context.Context, ref, selected, imageOS, arch string, content []byte, fetch func(context.Context, string) ([]byte, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !profile.IsExactRegistryImage(ref) {
		return execution.ErrInvalidSpec
	}
	if imageOS != "linux" || arch != "amd64" {
		return imageIdentityMismatch("unsupported cached platform")
	}
	var cached struct {
		ManifestDigest string `json:"manifest_digest"`
		ConfigDigest   string `json:"config_digest"`
		Manifest       string `json:"raw_manifest_json"`
		Config         string `json:"raw_config_json"`
	}
	if len(content) > imageMetadataLimit || json.Unmarshal(content, &cached) != nil {
		return errImageCorrupt
	}
	manifestBytes, configBytes := []byte(cached.Manifest), []byte(cached.Config)
	if cached.ManifestDigest != selected || digest.Digest(selected).Validate() != nil || digest.Digest(selected).Algorithm() != digest.SHA256 {
		return imageIdentityMismatch("cached manifest bytes do not match SDK content identity")
	}
	if !matchesImageDigest(selected, manifestBytes) {
		// Registry::fetch_manifest_and_config in SDK 0.7.6 reserializes the
		// parsed OCI manifest. Authenticate the original registry bytes, then
		// prove equivalence to the cached content rather than hashing that
		// reserialization and treating a different hash as a new identity.
		originalRef := strings.Split(ref, "@")[0] + "@" + selected
		original, err := fetch(ctx, originalRef)
		if err != nil {
			return errors.Join(ctx.Err(), errImageUnavailable)
		}
		if len(original) > imageMetadataLimit || !matchesImageDigest(selected, original) || !sameImageJSON(manifestBytes, original) {
			return imageIdentityMismatch("cached manifest differs from authenticated registry content")
		}
		manifestBytes = original
	}
	var manifest oci.Manifest
	if json.Unmarshal(manifestBytes, &manifest) != nil || manifest.SchemaVersion != 2 || !imageManifestType(manifest.MediaType) {
		return errImageCorrupt
	}
	if cached.ConfigDigest != manifest.Config.Digest.String() || !matchesImageDigest(cached.ConfigDigest, configBytes) || manifest.Config.Size != int64(len(configBytes)) {
		return imageIdentityMismatch("cached config bytes do not match manifest")
	}
	var config oci.Image
	if json.Unmarshal(configBytes, &config) != nil {
		return errImageCorrupt
	}
	if config.OS != imageOS || config.Architecture != arch || config.Variant != "" || config.OSVersion != "" || len(config.OSFeatures) != 0 {
		return imageIdentityMismatch("config platform does not match SDK platform")
	}
	named, _ := reference.ParseNamed(ref) // Validated above.
	wanted := named.(reference.Canonical).Digest().String()
	if wanted == selected {
		return nil // Imported platform-manifest fixtures need no registry access.
	}
	indexBytes, err := fetch(ctx, ref)
	if err != nil {
		// Fetch errors can contain credentials, URLs, or provider response bodies.
		return errors.Join(ctx.Err(), errImageUnavailable)
	}
	if len(indexBytes) > imageMetadataLimit || !matchesImageDigest(wanted, indexBytes) {
		return imageIdentityMismatch("index bytes do not match pinned digest")
	}
	var index oci.Index
	if json.Unmarshal(indexBytes, &index) != nil || index.SchemaVersion != 2 || (index.MediaType != oci.MediaTypeImageIndex && index.MediaType != "application/vnd.docker.distribution.manifest.list.v2+json") {
		return imageIdentityMismatch("pinned artifact is not a supported image index")
	}
	// Match the selected manifest to its own platform descriptor, not merely
	// another linux/amd64 entry somewhere in the same index.
	for _, descriptor := range index.Manifests {
		if descriptor.Digest.String() == selected && descriptor.Size == int64(len(manifestBytes)) && imageManifestType(descriptor.MediaType) && descriptor.MediaType == manifest.MediaType && plainImagePlatform(descriptor.Platform, imageOS, arch) {
			return nil
		}
	}
	return imageIdentityMismatch("index does not link selected manifest to qualified platform")
}

func imageIdentityMismatch(reason string) error {
	return fmt.Errorf("%w: Microsandbox %s", execution.ErrBindingMismatch, reason)
}

func plainImagePlatform(platform *oci.Platform, imageOS, arch string) bool {
	return platform != nil && platform.OS == imageOS && platform.Architecture == arch && platform.Variant == "" && platform.OSVersion == "" && len(platform.OSFeatures) == 0
}

func imageManifestType(mediaType string) bool {
	return mediaType == oci.MediaTypeImageManifest || mediaType == "application/vnd.docker.distribution.manifest.v2+json"
}

func matchesImageDigest(expected string, content []byte) bool {
	value := digest.Digest(expected)
	return value.Validate() == nil && value.Algorithm() == digest.SHA256 && digest.FromBytes(content) == value
}

func sameImageJSON(left, right []byte) bool {
	decode := func(content []byte) (any, error) {
		decoder := json.NewDecoder(bytes.NewReader(content))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return nil, errImageCorrupt
		}
		return value, nil
	}
	first, err := decode(left)
	if err != nil {
		return false
	}
	second, err := decode(right)
	return err == nil && reflect.DeepEqual(first, second)
}
