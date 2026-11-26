package main

import (
	"encoding/json"
	piresources "github.com/jozala/omnigrex/agent/pi"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Qualification fixtures must not change when production adopts a new bundle.
func fixtureBundle(t *testing.T, version int) piresources.Bundle {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "agent", "pi", "testdata", "bundle-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := piresources.ParseBundle(data)
	if err != nil {
		t.Fatal(err)
	}
	switch version {
	case 1:
	case 2:
		bundle.Version = 2
		bundle.SourceCommit = strings.Repeat("c", 40)
		bundle.Image = "ghcr.io/jozala/omnigrex/pi@sha256:" + strings.Repeat("c", 64)
		bundle.Qualification = piresources.BundleQualification{Commit: strings.Repeat("d", 40), Suite: "pi-bundle/v1", RunID: 600, Attempt: 2, DockerJobID: 601, MicrosandboxJobID: 602}
		var inventory piresources.ResourceContract
		if err := json.Unmarshal(bundle.Resources, &inventory); err != nil {
			t.Fatal(err)
		}
		for i := range inventory.Files {
			if inventory.Files[i].Name == "runner.mjs" {
				inventory.Files[i].SHA256 = strings.Repeat("c", 64)
			}
		}
		inventory.Files = append(inventory.Files, piresources.ResourceFile{Name: "candidate-only.mjs", SHA256: strings.Repeat("c", 64)})
		bundle.Resources = mustJSON(t, inventory)
	default:
		t.Fatalf("unsupported fixture bundle version %d", version)
	}
	bundle, err = piresources.ParseBundle(mustJSON(t, bundle))
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}
