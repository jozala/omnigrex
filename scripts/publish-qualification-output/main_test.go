package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/dirstate"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

func TestPublishQualificationOutputsAreValidatedPrivateAndExclusive(t *testing.T) {
	for kind, contents := range qualificationOutputs(t) {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			input, output := filepath.Join(root, "input"), filepath.Join(root, "output")
			if err := os.WriteFile(input, contents, 0644); err != nil {
				t.Fatal(err)
			}
			if err := publish(t.Context(), kind, input, output); err != nil {
				t.Fatal(err)
			}
			actual, err := os.ReadFile(output)
			if err != nil || !bytes.Equal(actual, contents) {
				t.Fatalf("published content changed: %v", err)
			}
			info, err := os.Stat(output)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("published mode is not private: %v", err)
			}
			if err := publish(t.Context(), kind, input, output); err == nil {
				t.Fatal("existing output overwritten")
			}
			for _, target := range []string{filepath.Join(root, "directory"), filepath.Join(root, "symlink")} {
				if filepath.Base(target) == "directory" {
					if err := os.Mkdir(target, 0700); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Symlink(output, target); err != nil {
						t.Fatal(err)
					}
				}
				if err := publish(t.Context(), kind, input, target); err == nil {
					t.Fatal("existing non-file destination accepted")
				}
			}
			entries, err := filepath.Glob(filepath.Join(root, ".pi-qualification-output-*"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("staging was retained: %v %v", entries, err)
			}
		})
	}
}

func TestPublishRejectsEmptyCorruptAndTruncatedOutputs(t *testing.T) {
	for kind, contents := range qualificationOutputs(t) {
		for name, bad := range map[string][]byte{"empty": nil, "corrupt": []byte("partial"), "truncated": contents[:len(contents)-1]} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				root := t.TempDir()
				input, output := filepath.Join(root, "input"), filepath.Join(root, "output")
				if err := os.WriteFile(input, bad, 0600); err != nil {
					t.Fatal(err)
				}
				if err := publish(t.Context(), kind, input, output); err == nil {
					t.Fatal("invalid output published")
				}
				if _, err := os.Lstat(output); !os.IsNotExist(err) {
					t.Fatalf("invalid output created final path: %v", err)
				}
			})
		}
	}
}

func qualificationOutputs(t *testing.T) map[string][]byte {
	t.Helper()
	platform := profile.Platform{OS: "linux", Arch: "amd64"}
	source, err := profile.NewPiV1("registry.example/pi@sha256:"+strings.Repeat("a", 64), platform)
	if err != nil {
		t.Fatal(err)
	}
	target, err := profile.NewPiV1("registry.example/pi@sha256:"+strings.Repeat("b", 64), platform)
	if err != nil {
		t.Fatal(err)
	}
	result := profile.CompatibilityResult{Source: source.Binding(), Target: target.Binding(), Platform: platform, StateContractVersion: profile.PiStateContractVersion, WorkspacePath: "/workspace", QualificationSuite: profile.PiCompatibilityQualificationSuite, QualificationVersion: "1", QualifiedAt: "2026-10-10T12:00:00Z", Outcome: "success", BackendFamily: "docker", BackendContractVersion: 1}
	compatibility, err := json.Marshal(profile.CompatibilityResultsFile{SchemaVersion: profile.PiCompatibilityResultsSchemaVersion, Results: []profile.CompatibilityResult{result}})
	if err != nil {
		t.Fatal(err)
	}
	ref := execution.StateRef{ID: "10000000-0000-4000-8000-000000000001", ParticipantID: "10000000-0000-4000-8000-000000000001", Backend: execution.BackendBinding{Family: "docker", Version: 1, Namespace: "qualification-only"}, Format: profile.PiStateContractVersion, RuntimeProfileSHA256: source.ContentSHA256(), Image: source.Binding().Image}
	root := t.TempDir()
	state, err := dirstate.New(root, ref.Backend, publicationStateMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Ensure(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "assignment-"+ref.ID, "runtime-state", "history.jsonl")
	if err := os.WriteFile(path, []byte("retained history\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := state.Export(t.Context(), ref, &archive); err != nil {
		t.Fatal(err)
	}
	return map[string][]byte{"compatibility": compatibility, "state": archive.Bytes()}
}

type publicationStateMetadata struct{}

func (publicationStateMetadata) StateMetadata(_ context.Context, ref execution.StateRef) (json.RawMessage, bool, error) {
	content, err := json.Marshal(struct {
		Version int    `json:"version"`
		Subpath string `json:"subpath"`
	}{1, "assignment-" + ref.ID + "/runtime-state"})
	return content, false, err
}
