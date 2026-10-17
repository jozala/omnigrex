package profile_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/profile"
)

func TestCompatibilityResultsFileRoundTrip(t *testing.T) {
	result := compatibilityResult(t)
	encoded, err := json.Marshal(profile.CompatibilityResultsFile{
		SchemaVersion: profile.CompatibilityResultsSchemaVersion,
		Results:       []profile.CompatibilityResult{result},
	})
	if err != nil {
		t.Fatalf("encode compatibility results: %v", err)
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
		{name: "source tag", mutate: func(result *profile.CompatibilityResult) { result.Source.Image = "registry.example/pi:old" }},
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
			if err := result.Validate(); err == nil {
				t.Fatal("CompatibilityResult.Validate() error = nil, want validation error")
			}
		})
	}
}

func TestCompatibilityResultsValidateConfiguredTarget(t *testing.T) {
	target, err := profile.NewPiV2(
		"registry.example/omnigrex/pi@sha256:"+strings.Repeat("b", 64),
		profile.Platform{OS: "linux", Arch: "amd64"},
	)
	if err != nil {
		t.Fatal(err)
	}
	source, err := profile.NewPiV2(
		"registry.example/omnigrex/pi@sha256:"+strings.Repeat("a", 64),
		profile.Platform{OS: "linux", Arch: "amd64"},
	)
	if err != nil {
		t.Fatal(err)
	}
	result := newCompatibilityResult(source.Binding(), target.Binding(), target.Contract().Platform, time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC))
	file := profile.CompatibilityResultsFile{SchemaVersion: profile.CompatibilityResultsSchemaVersion, Results: []profile.CompatibilityResult{result}}
	if err := file.ValidateTarget(target); err != nil {
		t.Fatalf("ValidateTarget() error = %v", err)
	}
	other, err := profile.NewPiV2(
		"registry.example/omnigrex/pi@sha256:"+strings.Repeat("c", 64),
		profile.Platform{OS: "linux", Arch: "amd64"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.ValidateTarget(other); err == nil {
		t.Fatal("ValidateTarget() error = nil, want target mismatch")
	}
}

func TestDecodeCompatibilityResultsFileIsStrict(t *testing.T) {
	for _, content := range []string{
		`{}`,
		`{"schema_version":"omnigrex.runtime-profile-compatibility-results/v2","results":[],"unknown":true}`,
		`{"schema_version":"omnigrex.runtime-profile-compatibility-results/v2","results":[]} trailing`,
	} {
		if _, err := profile.DecodeCompatibilityResultsFile(strings.NewReader(content)); err == nil {
			t.Fatalf("DecodeCompatibilityResultsFile(%q) error = nil", content)
		}
	}
}

func TestPiCompatibilityResultsBindExactRuntimePlatformAndBackend(t *testing.T) {
	platform := profile.Platform{OS: "linux", Arch: "amd64"}
	source, err := profile.NewPiV2("registry.example/omnigrex/pi@sha256:"+strings.Repeat("a", 64), platform)
	if err != nil {
		t.Fatal(err)
	}
	target, err := profile.NewPiV2("registry.example/omnigrex/pi@sha256:"+strings.Repeat("b", 64), platform)
	if err != nil {
		t.Fatal(err)
	}
	result := profile.CompatibilityResult{Source: source.Binding(), Target: target.Binding(), Platform: platform, StateContractVersion: profile.PiStateContractVersion, WorkspacePath: "/workspace", QualificationSuite: profile.PiCompatibilityQualificationSuite, QualificationVersion: "1", QualifiedAt: "2026-10-10T12:00:00Z", Outcome: "success", BackendFamily: "docker", BackendContractVersion: 1}
	file := profile.CompatibilityResultsFile{SchemaVersion: profile.PiCompatibilityResultsSchemaVersion, Results: []profile.CompatibilityResult{result}}
	encoded, _ := json.Marshal(file)
	if _, err := profile.DecodeCompatibilityResultsFile(bytes.NewReader(encoded)); err != nil {
		t.Fatal(err)
	}
	if err := file.ValidateDeployment("docker", 1, target); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		family  string
		version int
		target  profile.Profile
	}{{"microsandbox", 1, target}, {"docker", 2, target}, {"docker", 1, source}} {
		if err := file.ValidateDeployment(tc.family, tc.version, tc.target); err == nil {
			t.Fatal("unqualified deployment accepted")
		}
	}
	for _, mutate := range []func(*profile.CompatibilityResult){
		func(r *profile.CompatibilityResult) { r.StateContractVersion = "unsupported-state/v1" },
		func(r *profile.CompatibilityResult) { r.Source.Name = "unsupported-runtime" },
		func(r *profile.CompatibilityResult) { r.BackendFamily = "" },
		func(r *profile.CompatibilityResult) { r.BackendContractVersion = 2 },
		func(r *profile.CompatibilityResult) { r.QualificationSuite = "unsupported-upgrade-suite" },
	} {
		bad := result
		mutate(&bad)
		if err := bad.Validate(); err == nil {
			t.Fatal("unsupported Pi state qualification accepted")
		}
	}
	other := result
	other.BackendFamily = "microsandbox"
	a, _ := result.QualificationKey()
	b, _ := other.QualificationKey()
	if a == b {
		t.Fatal("backend qualifications share identity")
	}
	file.SchemaVersion = "omnigrex.runtime-profile-compatibility-results/v1"
	if err := file.Validate(); err == nil {
		t.Fatal("legacy schema accepted Pi qualification")
	}
}

func compatibilityResult(t *testing.T) profile.CompatibilityResult {
	t.Helper()
	source, err := profile.NewPiV2(
		"registry.example/omnigrex/pi@sha256:"+strings.Repeat("a", 64),
		profile.Platform{OS: "linux", Arch: "amd64"},
	)
	if err != nil {
		t.Fatal(err)
	}
	target, err := profile.NewPiV2(
		"registry.example/omnigrex/pi@sha256:"+strings.Repeat("b", 64),
		profile.Platform{OS: "linux", Arch: "amd64"},
	)
	if err != nil {
		t.Fatal(err)
	}
	return newCompatibilityResult(source.Binding(), target.Binding(),
		profile.Platform{OS: "linux", Arch: "amd64"}, time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC))
}

func newCompatibilityResult(source, target profile.Binding, platform profile.Platform, qualifiedAt time.Time) profile.CompatibilityResult {
	return profile.CompatibilityResult{
		Source: source, Target: target, Platform: platform,
		StateContractVersion: profile.StateContractVersion, WorkspacePath: profile.StableWorkspacePath,
		QualificationSuite: profile.CompatibilityQualificationSuite, QualificationVersion: profile.CompatibilityQualificationVersion,
		QualifiedAt: qualifiedAt.UTC().Truncate(time.Second).Format(time.RFC3339), Outcome: "success",
		BackendFamily: "docker", BackendContractVersion: 1,
	}
}
