package piresources

import (
	"encoding/json"
	"strings"
	"testing"
)

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
	script, err := QualificationScript("amd64", nil)
	if err != nil || !strings.Contains(script, "runner.mjs") || strings.Contains(script, "temporary-filesystems.mjs") {
		t.Fatal("implicit qualification uses checkout candidate")
	}
	if _, err := QualificationScript("arm64", nil); err == nil {
		t.Fatal("missing production selection fell back to checkout")
	}
}

func TestCompleteSelectedResourcesRejectUnsafeExecutableRequirements(t *testing.T) {
	b, err := SelectedBundle("linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
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
