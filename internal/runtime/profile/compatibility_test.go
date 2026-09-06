package profile_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/profile"
)

func TestCompatibilityResultsFileRoundTrip(t *testing.T) {
	result := compatibilityResult(t)
	encoded, err := profile.EncodeCompatibilityResultsFile([]profile.CompatibilityResult{result})
	if err != nil {
		t.Fatalf("EncodeCompatibilityResultsFile() error = %v", err)
	}
	decoded, err := profile.DecodeCompatibilityResultsFile(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("DecodeCompatibilityResultsFile() error = %v", err)
	}
	if len(decoded.Results) != 1 || decoded.Results[0] != result {
		t.Fatalf("decoded results = %#v, want %#v", decoded.Results, []profile.CompatibilityResult{result})
	}
}

func TestCompatibilityResultsRejectMutableImagesAndMalformedContracts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*profile.CompatibilityResult)
	}{
		{name: "source tag", mutate: func(result *profile.CompatibilityResult) { result.Source.Image = "registry.example/opencode:old" }},
		{name: "target local ID", mutate: func(result *profile.CompatibilityResult) { result.Target.Image = "sha256:" + strings.Repeat("c", 64) }},
		{name: "source hash", mutate: func(result *profile.CompatibilityResult) { result.Source.ContentSHA256 = "ABC" }},
		{name: "platform", mutate: func(result *profile.CompatibilityResult) { result.Platform.Arch = "s390x" }},
		{name: "state contract", mutate: func(result *profile.CompatibilityResult) { result.StateContractVersion = "mutable" }},
		{name: "workspace", mutate: func(result *profile.CompatibilityResult) { result.WorkspacePath = "/tmp/workspace" }},
		{name: "suite", mutate: func(result *profile.CompatibilityResult) { result.QualificationSuite = "smoke-test" }},
		{name: "suite version", mutate: func(result *profile.CompatibilityResult) { result.QualificationVersion = "2" }},
		{name: "time", mutate: func(result *profile.CompatibilityResult) { result.QualifiedAt = "2026-09-06T12:00:00+00:00" }},
		{name: "outcome", mutate: func(result *profile.CompatibilityResult) { result.Outcome = "failure" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := compatibilityResult(t)
			test.mutate(&result)
			if _, err := profile.EncodeCompatibilityResultsFile([]profile.CompatibilityResult{result}); err == nil {
				t.Fatal("EncodeCompatibilityResultsFile() error = nil, want validation error")
			}
		})
	}
}

func TestDecodeCompatibilityResultsFileIsStrict(t *testing.T) {
	for _, content := range []string{
		`{}`,
		`{"schema_version":"omnigrex.runtime-profile-compatibility-results/v1","results":[],"unknown":true}`,
		`{"schema_version":"omnigrex.runtime-profile-compatibility-results/v1","results":[]} trailing`,
	} {
		if _, err := profile.DecodeCompatibilityResultsFile(strings.NewReader(content)); err == nil {
			t.Fatalf("DecodeCompatibilityResultsFile(%q) error = nil", content)
		}
	}
}

func compatibilityResult(t *testing.T) profile.CompatibilityResult {
	t.Helper()
	source, err := profile.NewOpenCodeV1(
		"registry.example/omnigrex/opencode@sha256:"+strings.Repeat("a", 64),
		profile.Platform{OS: "linux", Arch: "amd64"},
	)
	if err != nil {
		t.Fatal(err)
	}
	target, err := profile.NewOpenCodeV1(
		"registry.example/omnigrex/opencode@sha256:"+strings.Repeat("b", 64),
		profile.Platform{OS: "linux", Arch: "amd64"},
	)
	if err != nil {
		t.Fatal(err)
	}
	return profile.NewCompatibilityResult(source.Binding(), target.Binding(),
		profile.Platform{OS: "linux", Arch: "amd64"}, time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC))
}
