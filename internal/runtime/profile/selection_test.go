package profile_test

import (
	"bytes"
	"encoding/json"
	piresources "github.com/jozala/omnigrex/agent/pi"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	"strings"
	"testing"
)

func TestFreshProductionUsesSelectedNotCheckoutResources(t *testing.T) {
	b, err := piresources.SelectedBundle("linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	p, err := profile.NewPiV2(testImage, profile.Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.Contract().GoldenResourceContract, b.Resources) {
		t.Fatal("fresh profile adopted candidate resources")
	}
	selected, err := p.WithEnvironment("registry.example/project@sha256:"+strings.Repeat("b", 64), p.Contract().Platform, nil)
	if err != nil || !bytes.Equal(selected.Contract().GoldenResourceContract, b.Resources) {
		t.Fatalf("repository image did not use selection: %v", err)
	}
}

func TestFreshArm64RejectedButRetainedArm64Resolves(t *testing.T) {
	arm := profile.Platform{OS: "linux", Arch: "arm64"}
	if _, err := profile.NewPiV2(testImage, arm); err == nil || !strings.Contains(err.Error(), "no selected production Pi bundle") {
		t.Fatalf("fresh arm64 admission: %v", err)
	}
	p, err := profile.NewPiV2(testImage, profile.Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.WithEnvironment(testImage, arm, nil); err == nil {
		t.Fatal("platform change bypassed selection")
	}
	// Capture the existing supported arm64 contract independently of admission.
	c := p.Contract()
	c.Platform = arm
	c.GoldenResourceContract = piresources.CandidateContract("arm64")
	retained, err := profile.New(c)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := json.Marshal(retained.Contract())
	restored, err := profile.DecodeContract(content, retained.Binding())
	if err != nil || restored.Binding() != retained.Binding() || !bytes.Equal(restored.Contract().GoldenResourceContract, retained.Contract().GoldenResourceContract) {
		t.Fatalf("retained arm64 requires current selection: %v", err)
	}
}

func selectedResources(t *testing.T) json.RawMessage {
	t.Helper()
	b, err := piresources.SelectedBundle("linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	return b.Resources
}
