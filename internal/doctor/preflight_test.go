package doctor

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/agentinstructions"
	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/config"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/role"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestValidateDeveloperWebhookURL(t *testing.T) {
	if err := validateDeveloperWebhookURL("https://omnigrex.example/webhooks/github"); err != nil {
		t.Fatalf("valid webhook URL error = %v", err)
	}
	for _, value := range []string{
		"http://omnigrex.example/webhooks/github",
		"https://omnigrex.example/wrong",
		"https://localhost/webhooks/github",
		"https://127.0.0.1/webhooks/github",
		"https://10.0.0.1/webhooks/github",
		"https://omnigrex.local/webhooks/github",
		"https://omnigrex.example/webhooks/github?secret=value",
	} {
		if err := validateDeveloperWebhookURL(value); err == nil {
			t.Errorf("validateDeveloperWebhookURL(%q) error = nil", value)
		}
	}
}

func TestPiDoctorRegistryAndEffectiveCredentials(t *testing.T) {
	state := productionState{settings: config.Config{PiImage: "registry.example/pi@sha256:" + strings.Repeat("a", 64), PiPlatform: runtimeprofile.Platform{OS: "linux", Arch: "amd64"}}}
	if err := state.checkRuntimeProfile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := state.registry.Resolve("pi-rpc", "v1"); err == nil {
		t.Fatal("unconfigured runtime was admitted")
	}
	p, err := agentprofile.Parse(".omnigrex/team/reviewer.md", []byte("---\nname: reviewer\nrole: REVIEWER\nruntime: pi-rpc/v2\nmodel: openai/gpt-4o-mini\npermissions:\n  read: allow\n---\nReview the Change Proposal.\n"), role.BuiltinPolicyCatalog())
	if err != nil {
		t.Fatal(err)
	}
	definition, err := workflow.NewBuiltinDefinition(role.BuiltinCatalog())
	if err != nil {
		t.Fatal(err)
	}
	for _, auth := range []struct {
		value string
		valid bool
	}{
		{`{"openai":{"type":"api_key","key":"fixture-only"}}`, true},
		{`{"other":{"type":"api_key","key":"fixture-only"}}`, false},
		{`{"openai":{"type":"api","key":"legacy-format"}}`, false},
		{`{"openai":{"type":"api_key","key":"!executable"}}`, false},
	} {
		err := validateEffectiveProfile(state.registry, role.Reviewer, p, agentinstructions.Operator{}, definition, []byte(auth.value))
		if (err == nil) != auth.valid {
			t.Fatalf("effective credential validity = %v, want %t", err, auth.valid)
		}
	}
}

func TestEffectivePiProfileAcceptsComposedInstructionsWithinPiBudget(t *testing.T) {
	runtime, err := runtimeprofile.NewPiV2(
		"registry.example/omnigrex/pi@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		runtimeprofile.Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := runtimeprofile.NewRegistry(runtime)
	if err != nil {
		t.Fatal(err)
	}
	policies := role.BuiltinPolicyCatalog()
	definition, err := workflow.NewBuiltinDefinition(role.BuiltinCatalog())
	if err != nil {
		t.Fatal(err)
	}
	profile, err := agentprofile.Parse(".omnigrex/team/reviewer.md", []byte("---\nname: reviewer\nrole: REVIEWER\nruntime: pi-rpc/v2\nmodel: openai/gpt-4o-mini\npermissions:\n  read: allow\n---\n"+strings.Repeat("p", 75<<10)), policies)
	if err != nil {
		t.Fatal(err)
	}
	credentials := []byte(`{"openai":{"type":"api_key","key":"fixture-only"}}`)
	if err := validateEffectiveProfile(registry, role.Reviewer, profile, agentinstructions.Operator{}, definition, credentials); err != nil {
		t.Fatalf("Profile fits without operator additions: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "common.md"), []byte(strings.Repeat("o", 50<<10)), 0o600); err != nil {
		t.Fatal(err)
	}
	operator, err := agentinstructions.Load(dir, policies)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateEffectiveProfile(registry, role.Reviewer, profile, operator, definition, credentials); err != nil {
		t.Fatalf("valid Pi instructions were rejected by an obsolete environment-string limit: %v", err)
	}
}

func TestValidateReviewerWebhookConfiguration(t *testing.T) {
	tests := []struct {
		name          string
		configuration githubapi.AppWebhookConfig
		requestErr    error
		wantErr       bool
	}{
		{
			name:       "disabled webhook reported as not found",
			requestErr: &githubapi.APIError{StatusCode: http.StatusNotFound, Method: http.MethodGet, Path: "/app/hook/config"},
		},
		{name: "disabled webhook with empty configuration"},
		{name: "configured webhook", configuration: githubapi.AppWebhookConfig{URL: "https://omnigrex.example/webhooks/github"}, wantErr: true},
		{name: "permission failure", requestErr: &githubapi.APIError{StatusCode: http.StatusForbidden}, wantErr: true},
		{name: "non API failure", requestErr: errors.New("connection failed"), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateReviewerWebhookConfiguration(test.configuration, test.requestErr)
			if (err != nil) != test.wantErr {
				t.Errorf("validateReviewerWebhookConfiguration() error = %v, want error %t", err, test.wantErr)
			}
		})
	}
}
