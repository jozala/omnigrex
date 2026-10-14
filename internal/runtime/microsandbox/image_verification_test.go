//go:build microsandbox && cgo

package microsandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	digest "github.com/opencontainers/go-digest"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
)

func imageProofFixture(t *testing.T) (string, string, []byte, []byte) {
	t.Helper()
	config := `{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`
	manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":%q,"digest":%q,"size":%d},"layers":[]}`, oci.MediaTypeImageManifest, oci.MediaTypeImageConfig, digest.FromString(config), len(config))
	selected := digest.FromString(manifest).String()
	index := fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[{"mediaType":%q,"digest":%q,"size":%d,"platform":{"architecture":"amd64","os":"linux"}}]}`, oci.MediaTypeImageIndex, oci.MediaTypeImageManifest, selected, len(manifest))
	metadata, err := json.Marshal(map[string]string{"manifest_digest": selected, "config_digest": digest.FromString(config).String(), "raw_manifest_json": manifest, "raw_config_json": config})
	if err != nil {
		t.Fatal(err)
	}
	return "registry.example/fixture@" + digest.FromString(index).String(), selected, metadata, []byte(index)
}

func TestImmutableImageContentVerification(t *testing.T) {
	ref, selected, metadata, index := imageProofFixture(t)
	for _, test := range []struct {
		name     string
		ref      string
		selected string
		metadata []byte
		index    []byte
		want     error
	}{
		{"index links selected platform", ref, selected, metadata, index, nil},
		{"exact imported manifest", "registry.example/fixture@" + selected, selected, metadata, nil, nil},
		{"all-f imported alias", "registry.example/fixture@sha256:" + strings.Repeat("f", 64), selected, metadata, index, execution.ErrBindingMismatch},
		{"digest-shaped SDK claim without matching bytes", ref, "sha256:" + strings.Repeat("f", 64), metadata, index, execution.ErrBindingMismatch},
		{"missing SDK digest", ref, "", metadata, index, execution.ErrBindingMismatch},
		{"corrupt index bytes", ref, selected, metadata, append(append([]byte{}, index...), ' '), execution.ErrBindingMismatch},
		{"missing cached raw bytes", ref, selected, []byte(`{"manifest_digest":"` + selected + `"}`), index, execution.ErrBindingMismatch},
		{"malformed metadata", ref, selected, []byte(`{"key":"secret"`), index, errImageCorrupt},
		{"mutable reference", "registry.example/fixture:latest", selected, metadata, index, execution.ErrInvalidSpec},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			err := verifyImageContent(context.Background(), test.ref, test.selected, "linux", "amd64", test.metadata, func(context.Context, string) ([]byte, error) {
				calls++
				return test.index, nil
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("verification=%v, want %v", err, test.want)
			}
			if test.name == "exact imported manifest" && calls != 0 {
				t.Fatal("offline platform-manifest fixture contacted registry")
			}
		})
	}
}

func TestImmutableImageIndexMustLinkTheSelectedManifestPlatform(t *testing.T) {
	_, selected, metadata, validIndex := imageProofFixture(t)
	for _, test := range []struct{ name, index string }{
		{"wrong platform on selected descriptor", strings.ReplaceAll(string(validIndex), `"amd64"`, `"arm64"`)},
		{"right platform but wrong manifest", strings.ReplaceAll(string(validIndex), selected, "sha256:"+strings.Repeat("a", 64))},
		{"missing platform", strings.ReplaceAll(string(validIndex), `,"platform":{"architecture":"amd64","os":"linux"}`, "")},
		{"wrong descriptor size", strings.ReplaceAll(string(validIndex), `"size":`, `"size":1`)},
		{"unqualified platform variant", strings.ReplaceAll(string(validIndex), `"os":"linux"`, `"os":"linux","variant":"v3"`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			ref := "registry.example/fixture@" + digest.FromString(test.index).String()
			err := verifyImageContent(context.Background(), ref, selected, "linux", "amd64", metadata, func(context.Context, string) ([]byte, error) { return []byte(test.index), nil })
			if !errors.Is(err, execution.ErrBindingMismatch) {
				t.Fatalf("unlinked platform accepted: %v", err)
			}
		})
	}
	// A separate amd64 entry must not bless the arm64 selected descriptor.
	var index oci.Index
	if err := json.Unmarshal(validIndex, &index); err != nil {
		t.Fatal(err)
	}
	other := index.Manifests[0]
	other.Digest = digest.FromString("other manifest")
	index.Manifests[0].Platform = &oci.Platform{OS: "linux", Architecture: "arm64"}
	index.Manifests = append(index.Manifests, other)
	content, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	ref := "registry.example/fixture@" + digest.FromBytes(content).String()
	if err := verifyImageContent(context.Background(), ref, selected, "linux", "amd64", metadata, func(context.Context, string) ([]byte, error) { return content, nil }); !errors.Is(err, execution.ErrBindingMismatch) {
		t.Fatalf("another descriptor blessed selected wrong platform: %v", err)
	}
}

func TestImmutableImageConfigBytesEstablishPlatform(t *testing.T) {
	_, selected, metadata, _ := imageProofFixture(t)
	var cached map[string]string
	if err := json.Unmarshal(metadata, &cached); err != nil {
		t.Fatal(err)
	}
	cached["raw_config_json"] = strings.ReplaceAll(cached["raw_config_json"], "amd64", "arm64")
	content, err := json.Marshal(cached)
	if err != nil {
		t.Fatal(err)
	}
	ref := "registry.example/fixture@" + selected
	if err := verifyImageContent(context.Background(), ref, selected, "linux", "amd64", content, nil); !errors.Is(err, execution.ErrBindingMismatch) {
		t.Fatalf("config byte corruption accepted: %v", err)
	}
	// Even a correctly rehashed arm64 config cannot be blessed by SDK amd64 metadata.
	cached["raw_manifest_json"] = strings.ReplaceAll(cached["raw_manifest_json"], cached["config_digest"], digest.FromString(cached["raw_config_json"]).String())
	cached["config_digest"] = digest.FromString(cached["raw_config_json"]).String()
	cached["manifest_digest"] = digest.FromString(cached["raw_manifest_json"]).String()
	content, err = json.Marshal(cached)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyImageContent(context.Background(), "registry.example/fixture@"+cached["manifest_digest"], cached["manifest_digest"], "linux", "amd64", content, nil); !errors.Is(err, execution.ErrBindingMismatch) {
		t.Fatalf("SDK platform claim overrode raw config: %v", err)
	}
}

func TestImmutableImageReserializedCacheRequiresAuthenticatedEquivalentBytes(t *testing.T) {
	_, selected, metadata, index := imageProofFixture(t)
	var cached map[string]string
	if err := json.Unmarshal(metadata, &cached); err != nil {
		t.Fatal(err)
	}
	original := []byte(cached["raw_manifest_json"])
	// The SDK's serialization is not the registry's original byte encoding.
	cached["raw_manifest_json"] = "\n " + cached["raw_manifest_json"] + "\n"
	reserialized, err := json.Marshal(cached)
	if err != nil {
		t.Fatal(err)
	}
	ref := "registry.example/fixture@" + selected
	calls := 0
	fetch := func(_ context.Context, requested string) ([]byte, error) {
		calls++
		if requested != ref {
			t.Fatalf("fetched something other than selected immutable manifest: %s", requested)
		}
		return original, nil
	}
	if err := verifyImageContent(context.Background(), ref, selected, "linux", "amd64", reserialized, fetch); err != nil || calls != 1 {
		t.Fatalf("authenticated reserialization rejected: %v, calls=%d", err, calls)
	}
	// Correct digest-shaped metadata cannot bless a changed cached document.
	cached["raw_manifest_json"] = strings.ReplaceAll(cached["raw_manifest_json"], `"layers":[]`, `"layers":[{"digest":"sha256:`+strings.Repeat("a", 64)+`","size":1,"mediaType":"evil"}]`)
	changed, err := json.Marshal(cached)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyImageContent(context.Background(), ref, selected, "linux", "amd64", changed, fetch); !errors.Is(err, execution.ErrBindingMismatch) {
		t.Fatalf("changed cached manifest blessed by registry digest claim: %v", err)
	}
	if err := verifyImageContent(context.Background(), ref, selected, "linux", "amd64", reserialized, func(context.Context, string) ([]byte, error) { return index, nil }); !errors.Is(err, execution.ErrBindingMismatch) {
		t.Fatalf("unverified registry bytes accepted: %v", err)
	}
}

func TestImmutableImageFetchFailureIsSanitizedAndCancellationPreserved(t *testing.T) {
	ref, selected, metadata, _ := imageProofFixture(t)
	source := errors.New("provider config private-key guest-output")
	ctx, cancel := context.WithCancel(context.Background())
	err := verifyImageContent(ctx, ref, selected, "linux", "amd64", metadata, func(context.Context, string) ([]byte, error) { cancel(); return nil, source })
	if !errors.Is(err, context.Canceled) || !errors.Is(err, errImageUnavailable) || errors.Is(err, source) || strings.Contains(fmt.Sprintf("%+v", err), "private-key") {
		t.Fatalf("unsafe/unclassified image error: %v", err)
	}
}

func TestCachedImmutableImageReadsBoundedRawMetadata(t *testing.T) {
	_, selected, metadata, _ := imageProofFixture(t)
	ref := "registry.example/fixture@" + selected
	home := t.TempDir()
	directory := filepath.Join(home, "cache", "manifests")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, digest.FromString(ref).Encoded()+".json")
	if err := os.WriteFile(path, metadata, 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyCachedImage(context.Background(), home, ref, selected, "linux", "amd64"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", imageMetadataLimit+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyCachedImage(context.Background(), home, ref, selected, "linux", "amd64"); !errors.Is(err, errImageCorrupt) {
		t.Fatalf("oversized cache accepted: %v", err)
	}
}

func TestDirectSDKCreateValidatesImageBeforeNativeDispatch(t *testing.T) {
	for _, diagnostic := range []bool{false, true} {
		_, err := (sdkPlatform{}).Create(context.Background(), creation{Request: execution.Request{Image: "registry.example/fixture:latest"}, Diagnostic: diagnostic})
		if !errors.Is(err, execution.ErrInvalidSpec) {
			t.Fatalf("direct create bypassed immutable validation: %v", err)
		}
	}
}
