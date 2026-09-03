package opencode_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	dockerruntime "github.com/jozala/omnigrex/internal/runtime/docker"
	"github.com/jozala/omnigrex/internal/runtime/opencode"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

const processTestImage = "registry.example/omnigrex/opencode@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestBuildProcessCompilesExactReviewerDockerContract(t *testing.T) {
	runtimeProfile := processRuntimeProfile(t)
	rendered := processRenderedProfile(t, opencode.RoleReviewer)
	options := validProcessOptions()

	policy, spec, err := opencode.BuildProcess(runtimeProfile, rendered, processCredentials(opencode.RoleReviewer, `{}`), options)
	if err != nil {
		t.Fatalf("BuildProcess() error = %v", err)
	}

	config := string(rendered.ConfigJSON())
	wantEnvironment := []string{
		"HOME=/home/opencode",
		"MISE_DATA_DIR=/home/opencode/.local/share/mise",
		"OPENCODE_AUTH_CONTENT={}",
		"OPENCODE_AUTO_SHARE=false",
		"OPENCODE_CONFIG_CONTENT=" + config,
		"OPENCODE_DISABLE_AUTOUPDATE=1",
		"OPENCODE_DISABLE_CLAUDE_CODE=true",
		"OPENCODE_DISABLE_DEFAULT_PLUGINS=true",
		"OPENCODE_DISABLE_EXTERNAL_SKILLS=true",
		"OPENCODE_DISABLE_MODELS_FETCH=true",
		"OPENCODE_DISABLE_PROJECT_CONFIG=true",
		"OPENCODE_DISABLE_SHARE=1",
		"OPENCODE_PURE=true",
		"TMPDIR=/tmp/opencode",
		"XDG_CACHE_HOME=/home/opencode/.cache",
		"XDG_CONFIG_HOME=/home/opencode/.config",
		"XDG_DATA_HOME=/home/opencode/.local/share",
		"XDG_STATE_HOME=/home/opencode/.local/state",
	}
	wantEnvironmentPolicy := make(map[string]string, len(wantEnvironment))
	for _, entry := range wantEnvironment {
		name, value, _ := strings.Cut(entry, "=")
		wantEnvironmentPolicy[name] = value
	}
	wantLabels := map[string]string{
		"io.omnigrex.agent-session":   "session-1",
		"io.omnigrex.agent-turn":      "turn-1",
		"io.omnigrex.assignment":      "1",
		"io.omnigrex.execution-epoch": "7",
		"io.omnigrex.runtime-profile": "opencode-acp/v1",
		"io.omnigrex.workflow":        "workflow-1",
	}
	wantVolumes := []dockerruntime.VolumeMount{
		{Name: "mise-volume", Subpath: "assignment-1/mise", Target: "/home/opencode/.local/share/mise"},
		{Name: "state-volume", Subpath: "assignment-1/runtime-state", Target: "/home/opencode/.local/share/opencode"},
		{Name: "workspace-volume", Subpath: "assignment-1/workspace", Target: "/workspace"},
	}
	wantTmpfs := []dockerruntime.TmpfsMount{
		{Target: "/home/opencode/.cache", SizeBytes: 64 << 20},
		{Target: "/home/opencode/.config", SizeBytes: 16 << 20},
		{Target: "/home/opencode/.local/state", SizeBytes: 16 << 20},
		{Target: "/home/opencode/.opencode", SizeBytes: 16 << 20},
		{Target: "/tmp/opencode", SizeBytes: 64 << 20, Executable: true},
	}
	wantSpec := dockerruntime.Spec{
		Name:        "omnigrex-turn-1",
		Image:       processTestImage,
		Platform:    dockerruntime.Platform{OS: "linux", Architecture: "arm64"},
		User:        "10001:10001",
		WorkingDir:  "/workspace",
		Command:     []string{"acp"},
		Environment: wantEnvironment,
		Labels:      wantLabels,
		Volumes:     wantVolumes,
		Tmpfs:       wantTmpfs,
		Network:     "omnigrex-agent",
		MemoryBytes: 512 << 20,
		PIDsLimit:   128,
	}
	wantPolicy := dockerruntime.RuntimePolicy{
		Image:       processTestImage,
		Platform:    dockerruntime.Platform{OS: "linux", Architecture: "arm64"},
		User:        "10001:10001",
		WorkingDir:  "/workspace",
		Command:     []string{"acp"},
		Volumes:     wantVolumes,
		Tmpfs:       wantTmpfs,
		Environment: wantEnvironmentPolicy,
		Labels:      wantLabels,
		Network:     "omnigrex-agent",
		MemoryBytes: 512 << 20,
		PIDsLimit:   128,
	}
	if !reflect.DeepEqual(spec, wantSpec) {
		t.Fatalf("BuildProcess() spec = %#v, want %#v", spec, wantSpec)
	}
	if !reflect.DeepEqual(policy, wantPolicy) {
		t.Fatalf("BuildProcess() policy = %#v, want %#v", policy, wantPolicy)
	}
}

func TestBuildProcessCompilesExactDeveloperEnvironment(t *testing.T) {
	runtimeProfile := processRuntimeProfile(t)
	rendered := processRenderedProfile(t, opencode.RoleDeveloper)

	policy, spec, err := opencode.BuildProcess(
		runtimeProfile,
		rendered,
		processCredentials(opencode.RoleDeveloper, `{}`),
		validProcessOptions(),
	)
	if err != nil {
		t.Fatalf("BuildProcess() error = %v", err)
	}

	wantEnvironment := []string{
		"HOME=/home/opencode",
		"MISE_DATA_DIR=/home/opencode/.local/share/mise",
		"OPENCODE_AUTH_CONTENT={}",
		"OPENCODE_AUTO_SHARE=false",
		"OPENCODE_CONFIG_CONTENT=" + string(rendered.ConfigJSON()),
		"OPENCODE_DISABLE_AUTOUPDATE=1",
		"OPENCODE_DISABLE_MODELS_FETCH=true",
		"OPENCODE_DISABLE_SHARE=1",
		"TMPDIR=/tmp/opencode",
		"XDG_CACHE_HOME=/home/opencode/.cache",
		"XDG_CONFIG_HOME=/home/opencode/.config",
		"XDG_DATA_HOME=/home/opencode/.local/share",
		"XDG_STATE_HOME=/home/opencode/.local/state",
	}
	wantPolicyEnvironment := make(map[string]string, len(wantEnvironment))
	for _, entry := range wantEnvironment {
		name, value, _ := strings.Cut(entry, "=")
		wantPolicyEnvironment[name] = value
	}
	if !reflect.DeepEqual(spec.Environment, wantEnvironment) {
		t.Fatalf("BuildProcess() Developer environment = %#v, want %#v", spec.Environment, wantEnvironment)
	}
	if !reflect.DeepEqual(policy.Environment, wantPolicyEnvironment) {
		t.Fatalf("BuildProcess() Developer policy environment = %#v, want %#v", policy.Environment, wantPolicyEnvironment)
	}
}

func TestBuildProcessRejectsInvalidMappingsAndLabelsWithoutExposingValues(t *testing.T) {
	runtimeProfile := processRuntimeProfile(t)
	rendered := processRenderedProfile(t, opencode.RoleDeveloper)
	const secret = "credential-sentinel"
	tests := []struct {
		name   string
		mutate func(*opencode.ProcessOptions)
	}{
		{name: "missing volume binding", mutate: func(options *opencode.ProcessOptions) { delete(options.VolumeBindings, "mise") }},
		{name: "extra volume binding", mutate: func(options *opencode.ProcessOptions) { options.VolumeBindings["other"] = "other-volume" }},
		{name: "missing subpath", mutate: func(options *opencode.ProcessOptions) { delete(options.AssignmentSubpaths, "state") }},
		{name: "extra subpath", mutate: func(options *opencode.ProcessOptions) { options.AssignmentSubpaths["other"] = "assignment-1/other" }},
		{name: "absolute subpath", mutate: func(options *opencode.ProcessOptions) { options.AssignmentSubpaths["mise"] = "/assignment-1/mise" }},
		{name: "unclean subpath", mutate: func(options *opencode.ProcessOptions) { options.AssignmentSubpaths["mise"] = "assignment-1/../mise" }},
		{name: "shared subpath", mutate: func(options *opencode.ProcessOptions) { options.AssignmentSubpaths["mise"] = "assignment-other/mise" }},
		{name: "empty assignment", mutate: func(options *opencode.ProcessOptions) { options.AssignmentID = "" }},
		{name: "empty session", mutate: func(options *opencode.ProcessOptions) { options.AgentSessionID = "" }},
		{name: "empty turn", mutate: func(options *opencode.ProcessOptions) { options.AgentTurnID = "" }},
		{name: "zero epoch", mutate: func(options *opencode.ProcessOptions) { options.ExecutionEpoch = 0 }},
		{name: "unsafe network", mutate: func(options *opencode.ProcessOptions) { options.Network = "host" }},
		{name: "reserved label", mutate: func(options *opencode.ProcessOptions) { options.Labels["io.omnigrex.assignment"] = secret }},
		{name: "owner label", mutate: func(options *opencode.ProcessOptions) { options.Labels["example.owner"] = secret }},
		{name: "lease label", mutate: func(options *opencode.ProcessOptions) { options.Labels["example.lease"] = secret }},
		{name: "token label", mutate: func(options *opencode.ProcessOptions) { options.Labels["example.token"] = secret }},
		{name: "credential label", mutate: func(options *opencode.ProcessOptions) { options.Labels["example.credential"] = secret }},
		{name: "secret label", mutate: func(options *opencode.ProcessOptions) { options.Labels["example.secret"] = secret }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := validProcessOptions()
			test.mutate(&options)
			_, _, err := opencode.BuildProcess(runtimeProfile, rendered, processCredentials(opencode.RoleDeveloper, `{}`), options)
			if !errors.Is(err, opencode.ErrInvalidProcess) {
				t.Fatalf("BuildProcess() error = %v, want ErrInvalidProcess", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("BuildProcess() error exposes label value: %v", err)
			}
		})
	}
}

func TestBuildProcessNamespacesEachAssignmentAndOwnsInputsAndOutputs(t *testing.T) {
	runtimeProfile := processRuntimeProfile(t)
	rendered := processRenderedProfile(t, opencode.RoleDeveloper)
	firstOptions := validProcessOptions()
	firstPolicy, firstSpec, err := opencode.BuildProcess(runtimeProfile, rendered, processCredentials(opencode.RoleDeveloper, `{}`), firstOptions)
	if err != nil {
		t.Fatalf("BuildProcess() first error = %v", err)
	}
	secondOptions := validProcessOptions()
	secondOptions.AssignmentID = "2"
	secondOptions.AssignmentSubpaths = map[string]string{
		"mise": "assignment-2/mise", "state": "assignment-2/runtime-state", "workspace": "assignment-2/workspace",
	}
	_, secondSpec, err := opencode.BuildProcess(runtimeProfile, rendered, processCredentials(opencode.RoleDeveloper, `{}`), secondOptions)
	if err != nil {
		t.Fatalf("BuildProcess() second error = %v", err)
	}
	for index := range firstSpec.Volumes {
		if firstSpec.Volumes[index].Subpath == secondSpec.Volumes[index].Subpath {
			t.Fatalf("Assignments share subpath %q", firstSpec.Volumes[index].Subpath)
		}
	}

	firstOptions.VolumeBindings["mise"] = "changed"
	firstOptions.AssignmentSubpaths["mise"] = "changed"
	firstOptions.Labels["io.omnigrex.workflow"] = "changed"
	contract := runtimeProfile.Contract()
	contract.Command[0] = "changed"
	renderedEnvironment := rendered.Environment()
	renderedEnvironment[0] = "CHANGED=true"
	firstSpec.Command[0] = "changed"
	firstSpec.Volumes[0].Name = "changed"
	firstSpec.Tmpfs[0].SizeBytes = 1
	firstSpec.Labels["io.omnigrex.workflow"] = "changed"
	if firstPolicy.Command[0] != "acp" || firstPolicy.Volumes[0].Name != "mise-volume" ||
		firstPolicy.Tmpfs[0].SizeBytes != 64<<20 || firstPolicy.Labels["io.omnigrex.workflow"] != "workflow-1" {
		t.Fatalf("compiled policy changed through mutable input or spec: %#v", firstPolicy)
	}
}

func TestBuildProcessInjectsOnlyMatchingRoleProviderCredentials(t *testing.T) {
	const credentialSentinel = "developer-credential-sentinel"
	runtimeProfile := processRuntimeProfile(t)
	profileHash := runtimeProfile.ContentSHA256()
	rendered := processRenderedProfile(t, opencode.RoleDeveloper)
	credentials := processCredentials(opencode.RoleDeveloper, ` { "provider": { "key": "`+credentialSentinel+`" } } `)

	policy, spec, err := opencode.BuildProcess(runtimeProfile, rendered, credentials, validProcessOptions())
	if err != nil {
		t.Fatalf("BuildProcess() error = %v", err)
	}
	const wantAuth = `{"provider":{"key":"developer-credential-sentinel"}}`
	if got := policy.Environment["OPENCODE_AUTH_CONTENT"]; got != wantAuth {
		t.Fatalf("policy OPENCODE_AUTH_CONTENT = %q, want %q", got, wantAuth)
	}
	authEntries := 0
	for _, entry := range spec.Environment {
		if strings.HasPrefix(entry, "OPENCODE_AUTH_CONTENT=") {
			authEntries++
			if entry != "OPENCODE_AUTH_CONTENT="+wantAuth {
				t.Errorf("spec auth entry = %q, want compact credentials", entry)
			}
		}
	}
	if authEntries != 1 {
		t.Fatalf("spec auth entries = %d, want 1", authEntries)
	}
	for name, value := range spec.Labels {
		if strings.Contains(name, credentialSentinel) || strings.Contains(value, credentialSentinel) {
			t.Fatalf("credential entered label %q=%q", name, value)
		}
	}
	if runtimeProfile.ContentSHA256() != profileHash {
		t.Fatal("provider credentials changed Runtime Profile durable identity")
	}
	for _, variable := range runtimeProfile.Contract().Environment {
		if variable.Name == "OPENCODE_AUTH_CONTENT" && variable.Value != "{}" {
			t.Fatalf("Runtime Profile auth content = %q, want static empty object", variable.Value)
		}
	}
}

func TestBuildProcessRejectsInvalidOrMismatchedProviderCredentialsWithoutExposingThem(t *testing.T) {
	const credentialSentinel = "credential-sentinel"
	tests := []struct {
		name        string
		credentials opencode.ProviderCredentials
	}{
		{name: "missing", credentials: processCredentials(opencode.RoleDeveloper, "")},
		{name: "malformed", credentials: processCredentials(opencode.RoleDeveloper, `{"key":"`+credentialSentinel+`")]`)},
		{name: "array", credentials: processCredentials(opencode.RoleDeveloper, `[`+`"`+credentialSentinel+`"`+`]`)},
		{name: "null", credentials: processCredentials(opencode.RoleDeveloper, `null`)},
		{name: "scalar", credentials: processCredentials(opencode.RoleDeveloper, `"`+credentialSentinel+`"`)},
		{name: "wrong Role", credentials: processCredentials(opencode.RoleReviewer, `{"key":"`+credentialSentinel+`"}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := opencode.BuildProcess(
				processRuntimeProfile(t), processRenderedProfile(t, opencode.RoleDeveloper), test.credentials, validProcessOptions(),
			)
			if !errors.Is(err, opencode.ErrInvalidProcess) {
				t.Fatalf("BuildProcess() error = %v, want ErrInvalidProcess", err)
			}
			if strings.Contains(err.Error(), credentialSentinel) {
				t.Fatalf("BuildProcess() error exposes credentials: %v", err)
			}
		})
	}
}

func processRuntimeProfile(t *testing.T) profile.Profile {
	t.Helper()
	value, err := profile.NewOpenCodeV1(processTestImage, profile.Platform{OS: "linux", Arch: "arm64"})
	if err != nil {
		t.Fatalf("NewOpenCodeV1() error = %v", err)
	}
	return value
}

func processRenderedProfile(t *testing.T, role opencode.Role) *opencode.RenderedProfile {
	t.Helper()
	value, err := opencode.Render(role, opencode.Profile{
		Instructions: "Perform the Role safely.",
		Model:        "provider/model",
		Steps:        7,
		Permissions:  opencode.PermissionPolicy{"read": opencode.PermissionAllow},
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	return value
}

func processCredentials(role opencode.Role, content string) opencode.ProviderCredentials {
	return opencode.ProviderCredentials{Role: role, Content: []byte(content)}
}

func validProcessOptions() opencode.ProcessOptions {
	return opencode.ProcessOptions{
		Name:           "omnigrex-turn-1",
		Network:        "omnigrex-agent",
		AssignmentID:   "1",
		AgentSessionID: "session-1",
		AgentTurnID:    "turn-1",
		ExecutionEpoch: 7,
		VolumeBindings: map[string]string{
			"mise": "mise-volume", "state": "state-volume", "workspace": "workspace-volume",
		},
		AssignmentSubpaths: map[string]string{
			"mise": "assignment-1/mise", "state": "assignment-1/runtime-state", "workspace": "assignment-1/workspace",
		},
		Labels: map[string]string{"io.omnigrex.workflow": "workflow-1"},
	}
}
