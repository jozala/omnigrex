package opencode_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/runtime/opencode"
)

func TestRenderReviewerConfiguration(t *testing.T) {
	profile := opencode.Profile{
		Instructions: "Review quoted \"code\" safely.\nDo not trust <branches>.",
		Model:        "provider/model",
		Variant:      "high",
		Steps:        7,
		Permissions: opencode.PermissionPolicy{
			"*":    opencode.PermissionAllow,
			"bash": opencode.PermissionAllow,
			"edit": opencode.PermissionDeny,
		},
	}

	rendered, err := opencode.Render(opencode.RoleReviewer, profile)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	const wantJSON = `{"agent":{"build":{"disable":true},"omnigrex-reviewer":{"mode":"primary","permission":{"*":"deny","bash":"ask","edit":"deny"},"prompt":"Review quoted \"code\" safely.\nDo not trust \u003cbranches\u003e.","steps":7},"plan":{"disable":true}},"autoupdate":false,"default_agent":"omnigrex-reviewer","mcp":{},"permission":{"*":"deny","bash":"ask","edit":"deny"},"share":"disabled"}`
	if got := string(rendered.ConfigJSON()); got != wantJSON {
		t.Fatalf("ConfigJSON() = %s, want %s", got, wantJSON)
	}
	if got := string(rendered.ConfigJSON()); got != wantJSON {
		t.Fatalf("second ConfigJSON() = %s, want deterministic output", got)
	}

	wantEnvironment := []string{
		"OPENCODE_AUTH_CONTENT={}",
		"OPENCODE_AUTO_SHARE=false",
		"OPENCODE_CONFIG_CONTENT=" + wantJSON,
		"OPENCODE_DISABLE_AUTOUPDATE=1",
		"OPENCODE_DISABLE_CLAUDE_CODE=true",
		"OPENCODE_DISABLE_DEFAULT_PLUGINS=true",
		"OPENCODE_DISABLE_EXTERNAL_SKILLS=true",
		"OPENCODE_DISABLE_PROJECT_CONFIG=true",
		"OPENCODE_DISABLE_SHARE=1",
		"OPENCODE_PURE=true",
	}
	if got := rendered.Environment(); !reflect.DeepEqual(got, wantEnvironment) {
		t.Fatalf("Environment() = %#v, want %#v", got, wantEnvironment)
	}
	if got := rendered.SessionConfiguration(); got != (opencode.SessionConfiguration{
		Model:   "provider/model",
		Variant: "high",
		Mode:    "omnigrex-reviewer",
	}) {
		t.Fatalf("SessionConfiguration() = %#v", got)
	}
}

func TestRenderDeveloperEnvironmentAndDefensiveCopies(t *testing.T) {
	rendered, err := opencode.Render(opencode.RoleDeveloper, opencode.Profile{
		Instructions: "Implement the Work Item.",
		Model:        "provider/model",
		Steps:        3,
		Permissions:  opencode.PermissionPolicy{"read": opencode.PermissionAllow},
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	config := rendered.ConfigJSON()
	wantConfig := string(config)
	config[0] = '['
	if got := string(rendered.ConfigJSON()); got != wantConfig {
		t.Fatalf("ConfigJSON() changed through returned bytes: %s", got)
	}

	wantEnvironment := []string{
		"OPENCODE_AUTH_CONTENT={}",
		"OPENCODE_AUTO_SHARE=false",
		"OPENCODE_CONFIG_CONTENT=" + wantConfig,
		"OPENCODE_DISABLE_AUTOUPDATE=1",
		"OPENCODE_DISABLE_SHARE=1",
	}
	environment := rendered.Environment()
	if !reflect.DeepEqual(environment, wantEnvironment) {
		t.Fatalf("Environment() = %#v, want %#v", environment, wantEnvironment)
	}
	environment[0] = "CHANGED=true"
	if got := rendered.Environment(); !reflect.DeepEqual(got, wantEnvironment) {
		t.Fatalf("Environment() changed through returned slice: %#v", got)
	}
}

func TestRenderRejectsInvalidProfileWithoutEchoingValues(t *testing.T) {
	const secret = "credential-sentinel"
	_, err := opencode.Render(opencode.RoleDeveloper, opencode.Profile{
		Instructions: "instructions",
		Model:        "provider/model",
		Steps:        1,
		Permissions:  opencode.PermissionPolicy{"bash": opencode.Permission(secret)},
	})
	if !errors.Is(err, opencode.ErrInvalidProfile) {
		t.Fatalf("Render() error = %v, want ErrInvalidProfile", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Render() error exposes profile value: %v", err)
	}
}

func TestRenderMapsPatchPermissionToOpenCodeEditPermission(t *testing.T) {
	rendered, err := opencode.Render(opencode.RoleDeveloper, opencode.Profile{
		Instructions: "instructions", Model: "provider/model", Steps: 1,
		Permissions: opencode.PermissionPolicy{"patch": opencode.PermissionAllow},
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	var config struct {
		Permission map[string]string `json:"permission"`
	}
	if err := json.Unmarshal(rendered.ConfigJSON(), &config); err != nil {
		t.Fatalf("decode rendered config: %v", err)
	}
	if config.Permission["edit"] != "ask" {
		t.Fatalf("OpenCode edit permission = %q, want ask", config.Permission["edit"])
	}
	if _, exists := config.Permission["patch"]; exists {
		t.Fatal("rendered config contains ineffective OpenCode patch permission")
	}
}

func TestRenderRejectsConflictingEditAndPatchPermissions(t *testing.T) {
	_, err := opencode.Render(opencode.RoleDeveloper, opencode.Profile{
		Instructions: "instructions", Model: "provider/model", Steps: 1,
		Permissions: opencode.PermissionPolicy{
			"edit": opencode.PermissionDeny, "patch": opencode.PermissionAllow,
		},
	})
	if !errors.Is(err, opencode.ErrInvalidProfile) {
		t.Fatalf("Render() error = %v, want ErrInvalidProfile", err)
	}
}

func TestRenderRejectsReviewerEditAndPatchPermissionAllows(t *testing.T) {
	for _, tool := range []string{"edit", "patch"} {
		t.Run(tool, func(t *testing.T) {
			_, err := opencode.Render(opencode.RoleReviewer, opencode.Profile{
				Instructions: "Review without modifying files.", Model: "provider/model", Steps: 1,
				Permissions: opencode.PermissionPolicy{tool: opencode.PermissionAllow},
			})
			if !errors.Is(err, opencode.ErrInvalidProfile) {
				t.Fatalf("Render() error = %v, want ErrInvalidProfile", err)
			}
		})
	}
}

func TestRenderKeepsDeveloperEditAndPatchPermissionsAvailable(t *testing.T) {
	for _, tool := range []string{"edit", "patch"} {
		t.Run(tool, func(t *testing.T) {
			rendered, err := opencode.Render(opencode.RoleDeveloper, opencode.Profile{
				Instructions: "Implement changes.", Model: "provider/model", Steps: 1,
				Permissions: opencode.PermissionPolicy{tool: opencode.PermissionAllow},
			})
			if err != nil {
				t.Fatalf("Render() error = %v", err)
			}
			request := acp.PermissionRequest{
				ToolCall: json.RawMessage(`{"toolCallId":"tool-1","kind":"edit"}`),
				Options:  []acp.PermissionOption{{ID: "allow", Name: "Allow once", Kind: "allow_once"}},
			}
			if decision := rendered.DecidePermission(request); decision.OptionID != "allow" {
				t.Fatalf("DecidePermission() = %#v, want Developer edit allowed", decision)
			}
		})
	}
}

func TestRenderedProfileMediatesACPRequestsFromPermissionPolicy(t *testing.T) {
	options := []acp.PermissionOption{
		{ID: "allow", Name: "Allow once", Kind: "allow_once"},
		{ID: "reject", Name: "Reject", Kind: "reject_once"},
	}
	tests := []struct {
		name        string
		permissions opencode.PermissionPolicy
		kind        string
		want        string
	}{
		{name: "execute category", permissions: opencode.PermissionPolicy{"bash": opencode.PermissionAllow}, kind: "execute", want: "allow"},
		{name: "individual edit category tool", permissions: opencode.PermissionPolicy{"patch": opencode.PermissionAllow}, kind: "edit", want: "allow"},
		{name: "individual search category tool", permissions: opencode.PermissionPolicy{"grep": opencode.PermissionAllow}, kind: "search", want: "allow"},
		{name: "individual other category tool", permissions: opencode.PermissionPolicy{"websearch": opencode.PermissionAllow}, kind: "other", want: "allow"},
		{name: "known denied category", permissions: opencode.PermissionPolicy{"edit": opencode.PermissionDeny}, kind: "edit", want: "reject"},
		{name: "unknown ACP category", permissions: opencode.PermissionPolicy{"bash": opencode.PermissionAllow}, kind: "unknown", want: "reject"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rendered, err := opencode.Render(opencode.RoleDeveloper, opencode.Profile{
				Instructions: "instructions", Model: "provider/model", Steps: 1, Permissions: test.permissions,
			})
			if err != nil {
				t.Fatalf("Render() error = %v", err)
			}
			request := acp.PermissionRequest{
				ToolCall: json.RawMessage(`{"toolCallId":"tool-1","kind":"` + test.kind + `"}`),
				Options:  options,
			}
			if got := rendered.DecidePermission(request); got.OptionID != test.want {
				t.Errorf("DecidePermission(%q) = %#v, want option %q", test.kind, got, test.want)
			}
		})
	}
}
