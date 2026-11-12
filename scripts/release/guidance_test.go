package main

import (
	piresources "github.com/jozala/omnigrex/agent/pi"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func developmentDocument(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "development-environment.yaml")
	data, err := exec.Command("sh", "../write-development-environment.sh", "ghcr.io/jozala/omnigrex/development@sha256:"+strings.Repeat("d", 64)).Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestQualifiedGuidance(t *testing.T) {
	dir, m := qualified(t)
	dev := developmentDocument(t)
	data, err := guidance(dir, dev)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, backend := range []string{"docker", "microsandbox"} {
		product := "orchestrator"
		if backend == "microsandbox" {
			product += "-microsandbox"
		}
		want := "### " + backend + "\n\n```dotenv\nOMNIGREX_ORCHESTRATOR_IMAGE=" + m.Images[product] + "\nOMNIGREX_PI_IMAGE=" + m.Images["pi"] + "\nOMNIGREX_PI_PLATFORM=linux/amd64\n```"
		if !strings.Contains(text, want) {
			t.Fatalf("missing backend snippet: %s", text)
		}
	}
	for _, want := range []string{"CI run 17, attempt 2", m.Commit, "not a manually designated public alpha/stable release", "independently adopted", "not an automatic update for every managed repository", "do not replace your full configuration or credentials"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
	artifact, _ := os.ReadFile(dev)
	if !strings.Contains(text, "```yaml\n"+string(artifact)+"```") {
		t.Fatal("qualified artifact not reused")
	}
}

func TestGuidanceFailsClosed(t *testing.T) {
	for _, state := range []string{"unready", "missing job", "extra job", "missing metadata", "mismatched metadata", "missing development", "invalid development"} {
		t.Run(state, func(t *testing.T) {
			dir, m := qualified(t)
			dev := developmentDocument(t)
			switch state {
			case "unready":
				m.Qualification = nil
			case "missing job":
				delete(m.Qualification, "development-smoke-microsandbox")
			case "extra job":
				m.Qualification["other"] = 99
			case "missing metadata":
				os.Remove(filepath.Join(dir, "candidate.json"))
			case "mismatched metadata":
				m.Images["pi"] = "ghcr.io/jozala/omnigrex/pi@sha256:" + strings.Repeat("f", 64)
			case "missing development":
				os.Remove(dev)
			case "invalid development":
				os.WriteFile(dev, []byte("version: 1\nimage: mutable:latest\n"), 0600)
			}
			if state != "missing metadata" {
				if err := writeJSON(dir, "candidate.json", m); err != nil {
					t.Fatal(err)
				}
				if err := writeChecksums(dir); err != nil {
					t.Fatal(err)
				}
			}
			data, err := guidance(dir, dev)
			if err == nil || len(data) != 0 {
				t.Fatal("unsafe metadata produced guidance")
			}
		})
	}
}

func TestPublicationSummarySeparatesCandidate(t *testing.T) {
	selected := "ghcr.io/jozala/omnigrex/pi@sha256:" + strings.Repeat("a", 64)
	candidate := "ghcr.io/jozala/omnigrex/pi@sha256:" + strings.Repeat("b", 64)
	for _, fresh := range []string{candidate, ""} {
		data, err := exec.Command("sh", "../write-publication-summary.sh", selected, fresh, "docker-digest", "microsandbox-digest", "development-digest").Output()
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, want := range []string{"not deployment approval", "not use this table as a deployment recommendation", "Selected production Pi: `" + selected + "`"} {
			if !strings.Contains(text, want) {
				t.Errorf("missing %q", want)
			}
		}
		if fresh != "" && !strings.Contains(text, "Pi candidate — **Not selected for production**: `"+candidate+"`") {
			t.Fatal(text)
		}
		if fresh != "" && !strings.Contains(text, "Changing `.env` alone does not adopt") {
			t.Fatal(text)
		}
		if fresh == "" && strings.Contains(text, "Newly published Pi candidate") {
			t.Fatal("invented candidate")
		}
	}
}

// The source selection can change after packaging; rendering must still use
// only the selected bundle captured in this exact payload.
func TestGuidanceUsesPackagedSelection(t *testing.T) {
	b := fixtureBuild()
	bundle, err := piresources.SelectedBundle("linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	b.SchemaVersion = 2
	b.PiBundle = &bundle
	b.Images["pi"] = bundle.Image
	dir := filepath.Join(t.TempDir(), "candidate")
	if err := pack(dir, filepath.Join("..", ".."), b); err != nil {
		t.Fatal(err)
	}
	m, _, err := verify(dir)
	if err != nil {
		t.Fatal(err)
	}
	m.Qualification = evidence()
	if err := writeJSON(dir, "candidate.json", m); err != nil {
		t.Fatal(err)
	}
	if err := writeChecksums(dir); err != nil {
		t.Fatal(err)
	}
	data, err := guidance(dir, developmentDocument(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "OMNIGREX_PI_IMAGE="+bundle.Image+"\n") != 2 {
		t.Fatal("selected packaged digest lost")
	}
	// Even a checksummed manifest cannot replace the selected image without
	// matching its packaged build identity and selected bundle.
	m.Images["pi"] = "ghcr.io/jozala/omnigrex/pi@sha256:" + strings.Repeat("e", 64)
	if err := writeJSON(dir, "candidate.json", m); err != nil {
		t.Fatal(err)
	}
	if err := writeChecksums(dir); err != nil {
		t.Fatal(err)
	}
	if data, err := guidance(dir, developmentDocument(t)); err == nil || len(data) != 0 {
		t.Fatal("mismatched bundle accepted")
	}
}
