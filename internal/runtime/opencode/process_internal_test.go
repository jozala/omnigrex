package opencode

import (
	"errors"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/profile"
)

func TestBuildProcessMergesDuplicateEnvironmentAndRejectsConflicts(t *testing.T) {
	runtimeProfile, err := profile.NewOpenCodeV1(
		"registry.example/omnigrex/opencode@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		profile.Platform{OS: "linux", Arch: "arm64"},
	)
	if err != nil {
		t.Fatalf("NewOpenCodeV1() error = %v", err)
	}
	options := ProcessOptions{
		Name: "process", Network: "network", AssignmentID: "1", AgentSessionID: "session-1", AgentTurnID: "turn-1", ExecutionEpoch: 1,
		VolumeBindings: map[string]string{"mise": "mise", "state": "state", "workspace": "workspace"},
		AssignmentSubpaths: map[string]string{
			"mise": "assignment-1/mise", "state": "assignment-1/state", "workspace": "assignment-1/workspace",
		},
	}
	rendered := &RenderedProfile{role: RoleDeveloper, environ: []string{
		"OPENCODE_AUTH_CONTENT={}",
		"OPENCODE_AUTH_CONTENT={}",
		"OPENCODE_CONFIG_CONTENT={}",
		"OPENCODE_CONFIG_CONTENT={}",
	}}
	credentials := ProviderCredentials{Role: RoleDeveloper, Content: []byte(`{}`)}
	_, spec, err := BuildProcess(runtimeProfile, rendered, credentials, options)
	if err != nil {
		t.Fatalf("BuildProcess() duplicate equal values error = %v", err)
	}
	configCount := 0
	for _, entry := range spec.Environment {
		if strings.HasPrefix(entry, "OPENCODE_CONFIG_CONTENT=") {
			configCount++
		}
	}
	if configCount != 1 {
		t.Fatalf("OPENCODE_CONFIG_CONTENT entries = %d, want 1", configCount)
	}

	rendered.environ = append(rendered.environ, "HOME=credential-sentinel")
	_, _, err = BuildProcess(runtimeProfile, rendered, credentials, options)
	if !errors.Is(err, ErrInvalidProcess) {
		t.Fatalf("BuildProcess() conflicting values error = %v, want ErrInvalidProcess", err)
	}
	if strings.Contains(err.Error(), "credential-sentinel") {
		t.Fatalf("BuildProcess() error exposes environment value: %v", err)
	}
}

func TestBuildProcessRequiresOpenCodeConfigurationEnvironment(t *testing.T) {
	runtimeProfile, err := profile.NewOpenCodeV1(
		"registry.example/omnigrex/opencode@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		profile.Platform{OS: "linux", Arch: "arm64"},
	)
	if err != nil {
		t.Fatalf("NewOpenCodeV1() error = %v", err)
	}
	options := ProcessOptions{
		Name: "process", Network: "network", AssignmentID: "1", AgentSessionID: "session-1", AgentTurnID: "turn-1", ExecutionEpoch: 1,
		VolumeBindings: map[string]string{"mise": "mise", "state": "state", "workspace": "workspace"},
		AssignmentSubpaths: map[string]string{
			"mise": "assignment-1/mise", "state": "assignment-1/state", "workspace": "assignment-1/workspace",
		},
	}
	_, _, err = BuildProcess(runtimeProfile, &RenderedProfile{}, ProviderCredentials{Role: RoleDeveloper, Content: []byte(`{}`)}, options)
	if !errors.Is(err, ErrInvalidProcess) {
		t.Fatalf("BuildProcess() error = %v, want ErrInvalidProcess", err)
	}
}
