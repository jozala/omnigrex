package opencode_test

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/opencode"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

const processTestImage = "registry.example/omnigrex/opencode@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestBuildProcessPreservesRoleConfigurationAndLogicalAttachments(t *testing.T) {
	for _, role := range []opencode.Role{opencode.RoleDeveloper, opencode.RoleReviewer} {
		runtimeProfile := processRuntimeProfile(t)
		rendered := processRenderedProfile(t, role)
		options := validProcessOptions()
		options.Environment = map[string]string{"TMPDIR": "/home/opencode/.local/share/omnigrex-tool-data/turn/turn-1/build", "PATH": "/tools:/usr/bin"}
		spec, err := opencode.BuildProcess(runtimeProfile, rendered, processCredentials(role, `{}`), options)
		if err != nil {
			t.Fatal(err)
		}
		if spec.Key != options.Key || spec.Image != processTestImage || spec.WorkingDir != "/workspace" || spec.User != "10001:10001" || !slices.Equal(spec.Command, []string{"acp"}) {
			t.Fatalf("runtime request = %#v", spec)
		}
		for _, entry := range append(rendered.Environment(), "TMPDIR="+options.Environment["TMPDIR"], "PATH=/tools:/usr/bin", "OPENCODE_AUTH_CONTENT={}") {
			if !slices.Contains(spec.Environment, entry) {
				t.Errorf("missing environment entry %s", entry)
			}
		}
		wantAttachments := []execution.Attachment{
			{Resource: "mise", Target: "/home/opencode/.local/share/mise"},
			{Resource: "state", Target: "/home/opencode/.local/share/opencode"},
			{Resource: "tool-data", Target: "/home/opencode/.local/share/omnigrex-tool-data"},
			{Resource: "workspace", Target: "/workspace"},
		}
		if !reflect.DeepEqual(spec.Attachments, wantAttachments) {
			t.Fatalf("attachments = %#v", spec.Attachments)
		}
		if spec.MemoryBytes != options.MemoryBytes || spec.PIDsLimit != 128 || len(spec.TemporaryDirectories) != 5 {
			t.Fatalf("resource contract = %#v", spec)
		}
	}
}

func TestBuildProcessRejectsInvalidIdentityAndEnvironment(t *testing.T) {
	for _, mutate := range []func(*opencode.ProcessOptions){
		func(o *opencode.ProcessOptions) { o.Key.AssignmentID = "" },
		func(o *opencode.ProcessOptions) { o.Key.AssignmentID = "../other" },
		func(o *opencode.ProcessOptions) { o.Key.AgentSessionID = "" },
		func(o *opencode.ProcessOptions) { o.Key.AgentTurnID = "" },
		func(o *opencode.ProcessOptions) { o.Key.ExecutionEpoch = 0 },
		func(o *opencode.ProcessOptions) { o.Key.RuntimeProfileVersion = "other" },
		func(o *opencode.ProcessOptions) { o.MemoryBytes = -1 },
		func(o *opencode.ProcessOptions) { o.Environment = map[string]string{"HOME": "credential-sentinel"} },
		func(o *opencode.ProcessOptions) { o.Environment = map[string]string{"BAD=NAME": "credential-sentinel"} },
	} {
		options := validProcessOptions()
		mutate(&options)
		_, err := opencode.BuildProcess(processRuntimeProfile(t), processRenderedProfile(t, opencode.RoleDeveloper), processCredentials(opencode.RoleDeveloper, `{}`), options)
		if !errors.Is(err, opencode.ErrInvalidProcess) || strings.Contains(err.Error(), "credential-sentinel") {
			t.Fatalf("validation = %v", err)
		}
	}
}

func TestBuildProcessKeepsCredentialsOutOfIdentityAndBinding(t *testing.T) {
	runtimeProfile := processRuntimeProfile(t)
	binding := runtimeProfile.Binding()
	options := validProcessOptions()
	spec, err := opencode.BuildProcess(runtimeProfile, processRenderedProfile(t, opencode.RoleDeveloper), processCredentials(opencode.RoleDeveloper, ` {"provider":{"key":"credential-sentinel"}} `), options)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(spec.Environment, `OPENCODE_AUTH_CONTENT={"provider":{"key":"credential-sentinel"}}`) || spec.Key != options.Key || runtimeProfile.Binding() != binding {
		t.Fatal("credential injection changed binding or identity")
	}
	for _, credential := range []opencode.ProviderCredentials{
		processCredentials(opencode.RoleDeveloper, ""), processCredentials(opencode.RoleDeveloper, `null`),
		processCredentials(opencode.RoleDeveloper, `["credential-sentinel"]`), processCredentials(opencode.RoleDeveloper, `{"key":"credential-sentinel"`),
		processCredentials(opencode.RoleReviewer, `{"key":"credential-sentinel"}`),
	} {
		_, err := opencode.BuildProcess(runtimeProfile, processRenderedProfile(t, opencode.RoleDeveloper), credential, options)
		if !errors.Is(err, opencode.ErrInvalidProcess) || strings.Contains(err.Error(), "credential-sentinel") {
			t.Fatalf("credential validation = %v", err)
		}
	}
}

func processRuntimeProfile(t *testing.T) profile.Profile {
	t.Helper()
	value, err := profile.NewOpenCodeV1(processTestImage, profile.Platform{OS: "linux", Arch: "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	return value
}
func processRenderedProfile(t *testing.T, role opencode.Role) *opencode.RenderedProfile {
	t.Helper()
	value, err := opencode.Render(role, opencode.Profile{Instructions: "Perform the Role safely.", Model: "provider/model", Steps: 7, Permissions: opencode.PermissionPolicy{"read": opencode.PermissionAllow}})
	if err != nil {
		t.Fatal(err)
	}
	return value
}
func processCredentials(role opencode.Role, content string) opencode.ProviderCredentials {
	return opencode.ProviderCredentials{Role: role, Content: []byte(content)}
}
func validProcessOptions() opencode.ProcessOptions {
	return opencode.ProcessOptions{
		Key:        execution.Key{AssignmentID: "1", AgentSessionID: "session-1", AgentTurnID: "turn-1", ExecutionEpoch: 7, RuntimeProfileName: "opencode-acp", RuntimeProfileVersion: "v1"},
		WorkflowID: "workflow-1", MemoryBytes: 512 << 20,
	}
}
