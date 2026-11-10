//go:build integration && microsandbox && cgo

package microsandbox

import (
	"encoding/json"
	piresources "github.com/jozala/omnigrex/agent/pi"
	"github.com/jozala/omnigrex/internal/runtime/conformance"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	sdk "github.com/superradcompany/microsandbox/sdk/go"
	"testing"
)

func TestSelectedAndRetainedPiBundleResources(t *testing.T) {
	backend, _, ctx := nativeFixture(t)
	bundle, err := piresources.SelectedBundle("linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if err := (commandCLI{backend.options}).Run(ctx, nil, nil, "image", "pull", bundle.Image); err != nil {
		t.Fatal(err)
	}
	r1, err := profile.NewPiV2(bundle.Image, profile.Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	images, err := sdk.Image.Load(ctx, "/tmp/golden-image.tar", "registry.example/pi-candidate:fixture")
	if err != nil || len(images) == 0 {
		t.Fatalf("candidate import: %v", err)
	}
	image := "registry.example/pi-candidate@" + images[0].ManifestDigest()
	if _, err := sdk.Image.Load(ctx, "/tmp/golden-image.tar", image); err != nil {
		t.Fatal(err)
	}
	r2, err := profile.NewCandidatePiV2(image, profile.Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	conformance.PiBundleInventories(t, ctx, r1, r2, backend.CheckRepositoryEnvironment)
	var inventory piresources.ResourceContract
	_ = json.Unmarshal(r2.Contract().GoldenResourceContract, &inventory)
	inventory.Version = 1
	inventory.BootstrapContract = ""
	inventory.Executables = nil
	c := r2.Contract()
	c.GoldenResourceContract, _ = json.Marshal(inventory)
	legacy, err := profile.New(c)
	if err != nil {
		t.Fatal(err)
	}
	conformance.PiBundleInventories(t, ctx, r2, legacy, backend.CheckRepositoryEnvironment)
}
