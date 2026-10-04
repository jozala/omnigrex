package doctor

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/agentinstructions"
	"github.com/jozala/omnigrex/internal/agentprofile"
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

func TestEffectiveProfileChecksComposedInstructionSize(t *testing.T) {
	runtime, err := runtimeprofile.NewOpenCodeV1(
		"registry.example/omnigrex/opencode@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
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
	profile, err := agentprofile.Parse(".omnigrex/team/reviewer.md", []byte("---\nname: reviewer\nrole: REVIEWER\nruntime: opencode-acp/v1\nmodel: provider/model\nsteps: 1\npermissions:\n  read: allow\n---\n"+strings.Repeat("p", 75<<10)), policies)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateEffectiveProfile(registry, role.Reviewer, profile, agentinstructions.Operator{}, definition); err != nil {
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
	if err := validateEffectiveProfile(registry, role.Reviewer, profile, operator, definition); err == nil || !strings.Contains(err.Error(), "120 KiB") {
		t.Fatalf("preflight did not reject oversized composed instructions: %v", err)
	}
}

func TestACPContainerContractUsesConfiguredMemoryInSpecAndPolicy(t *testing.T) {
	profile, err := runtimeprofile.NewOpenCodeV1(
		"registry.example/omnigrex/opencode@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		runtimeprofile.Platform{OS: "linux", Arch: "amd64"},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, memory := range []int64{512 << 20, 1024 << 20} {
		policy, spec, err := acpContainerContract(profile, profile.Contract().Image, acpProbeOptions{Network: "omnigrex-agent", MemoryBytes: memory})
		if err != nil {
			t.Fatal(err)
		}
		if policy.MemoryBytes != memory || spec.MemoryBytes != memory || policy.PIDsLimit != 128 || spec.PIDsLimit != 128 {
			t.Errorf("ACP policy/spec memory = %d/%d, PIDs = %d/%d", policy.MemoryBytes, spec.MemoryBytes, policy.PIDsLimit, spec.PIDsLimit)
		}
	}
	if _, _, err := acpContainerContract(profile, profile.Contract().Image, acpProbeOptions{}); err == nil {
		t.Error("ACP probe accepted missing memory")
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
