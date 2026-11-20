package piresources

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func fixtureBundle(t *testing.T) Bundle {
	t.Helper()
	data, err := os.ReadFile("testdata/bundle-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := ParseBundle(data)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func TestSelectionIsDefensiveAndStrict(t *testing.T) {
	b, err := SelectedBundle("linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Bundle){
		func(b *Bundle) { b.Image = "ghcr.io/jozala/omnigrex/pi:latest" },
		func(b *Bundle) { b.SourceCommit = "unknown" },
		func(b *Bundle) { b.Qualification.Attempt = 0 },
		func(b *Bundle) { b.Resources = json.RawMessage(`{}`) },
	} {
		c := b
		mutate(&c)
		data, _ := json.Marshal(c)
		if _, err := ParseBundle(data); err == nil {
			t.Fatal("invalid selection accepted")
		}
	}
	b.Resources[0] = 'x'
	again, err := SelectedBundle("linux", "amd64")
	if err != nil || again.Resources[0] != '{' {
		t.Fatal("selection mutated")
	}
	for _, platform := range [][2]string{{"linux", "arm64"}, {"darwin", "amd64"}, {"linux", "unknown"}} {
		if _, err := SelectedBundle(platform[0], platform[1]); err == nil {
			t.Fatal("unsupported production platform selected")
		}
	}
}

func TestQualificationUsesOnlyBoundInventory(t *testing.T) {
	bundle := fixtureBundle(t)
	script, err := QualificationScript("amd64", bundle.Resources)
	if err != nil {
		t.Fatal(err)
	}
	var inventory ResourceContract
	if err := json.Unmarshal(bundle.Resources, &inventory); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, inventory.NodeVersion) || !strings.Contains(script, inventory.MiseSHA256) {
		t.Fatal("qualification replaced bound tool versions")
	}
	for _, file := range inventory.Files {
		want := "printf '%s  %s\\n' '" + file.SHA256 + "' '/opt/omnigrex/pi/" + file.Name + "' | sha256sum -c -"
		if !strings.Contains(script, want) {
			t.Fatalf("qualification omitted bound checksum for %s", file.Name)
		}
	}
	if got := strings.Count(script, "omnigrex_check='owned checksum "); got != len(inventory.Files) {
		t.Fatalf("qualification checks %d files, want only the %d bound files", got, len(inventory.Files))
	}
	if _, err := QualificationScript("amd64", nil); err == nil {
		t.Fatal("missing retained inventory reconstructed from selection")
	}
}

func TestCompleteResourcesRejectUnsafeExecutableRequirements(t *testing.T) {
	b := fixtureBundle(t)
	for _, mutate := range []func(*ResourceContract){
		func(c *ResourceContract) { c.BootstrapContract = "omnigrex-environment/v2" },
		func(c *ResourceContract) { c.Executables = nil },
		func(c *ResourceContract) { c.Executables[0] = "/tmp/runner; touch /tmp/poison" },
		func(c *ResourceContract) { c.Version = 1 },
	} {
		var c ResourceContract
		_ = json.Unmarshal(b.Resources, &c)
		mutate(&c)
		content, _ := json.Marshal(c)
		if _, err := NormalizeContract(content); err == nil {
			t.Fatal("unsafe/incomplete executable requirement accepted")
		}
	}
}
