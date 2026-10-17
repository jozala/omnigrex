package profile_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/profile"
)

type contractSource struct {
	content json.RawMessage
	err     error
}

func (source contractSource) RuntimeProfileContract(context.Context, profile.Binding) (json.RawMessage, error) {
	return source.content, source.err
}

func TestPersistedRepositoryContractSurvivesCurrentImageAndRequirementChanges(t *testing.T) {
	current, err := profile.NewPiV2(testImage, profile.Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := current.WithEnvironment("registry.example/repository@sha256:"+strings.Repeat("b", 64), profile.Platform{OS: "linux", Arch: "arm64"}, []string{"nested-containers", "tool-installation"})
	if err != nil {
		t.Fatal(err)
	}
	content, _ := json.Marshal(bound.Contract())
	catalog, _ := profile.NewCatalog([]profile.Profile{current}, nil)
	resolver := profile.Resolver{Catalog: catalog, Source: contractSource{content: content}}
	resolved, err := resolver.ResolveBinding(bound.Binding())
	if err != nil || resolved.Binding() != bound.Binding() || resolved.Contract().Platform.Arch != "arm64" {
		t.Fatalf("historical resolution: %+v %v", resolved.Contract(), err)
	}
	corrupted := strings.Replace(string(content), "nested-containers", "hardware-isolation", 1)
	resolver.Source = contractSource{content: json.RawMessage(corrupted)}
	if _, err := resolver.ResolveBinding(bound.Binding()); !errors.Is(err, profile.ErrConflict) {
		t.Fatalf("tampered contract: %v", err)
	}
	resolver.Source = contractSource{err: errors.New("database unavailable")}
	if _, err := resolver.ResolveBinding(current.Binding()); err == nil {
		t.Fatal("source failure silently fell back")
	}
	resolver.Source = contractSource{err: profile.ErrNotFound}
	if _, err := resolver.ResolveBinding(bound.Binding()); !errors.Is(err, profile.ErrNotFound) {
		t.Fatalf("missing ledger definition fabricated historical content: %v", err)
	}
	if resolved, err := resolver.ResolveBinding(current.Binding()); err != nil || resolved.Binding() != current.Binding() {
		t.Fatalf("explicit current contract unavailable: %+v %v", resolved.Binding(), err)
	}
}
