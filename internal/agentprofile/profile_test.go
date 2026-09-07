package agentprofile_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/jozala/omnigrex/internal/agentprofile"
)

func TestParseReturnsValidatedDeveloperProfile(t *testing.T) {
	content := []byte("---\nruntime: opencode-acp/v1\nmodel: openai/gpt-5.2\nvariant: high\nsteps: 250\npermissions:\n  read: allow\n  edit: deny\n---\nImplement the Work Item carefully.\n")

	profile, err := agentprofile.Parse(agentprofile.Developer, content)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if profile.Name() != agentprofile.Developer || profile.Role() != agentprofile.RoleDeveloper {
		t.Errorf("identity = (%q, %q), want (%q, %q)", profile.Name(), profile.Role(), agentprofile.Developer, agentprofile.RoleDeveloper)
	}
	if profile.Path() != ".omnigrex/team/developer.md" {
		t.Errorf("Path() = %q", profile.Path())
	}
	if profile.Runtime() != "opencode-acp/v1" || profile.Model() != "openai/gpt-5.2" || profile.Variant() != "high" || profile.Steps() != 250 {
		t.Errorf("configuration = (%q, %q, %q, %d)", profile.Runtime(), profile.Model(), profile.Variant(), profile.Steps())
	}
	if profile.Instructions() != "Implement the Work Item carefully.\n" {
		t.Errorf("Instructions() = %q", profile.Instructions())
	}
	wantPermissions := map[string]agentprofile.PermissionAction{"read": agentprofile.Allow, "edit": agentprofile.Deny}
	if !reflect.DeepEqual(profile.Permissions(), wantPermissions) {
		t.Errorf("Permissions() = %#v", profile.Permissions())
	}
	permissions := profile.Permissions()
	permissions["bash"] = agentprofile.Allow
	if _, exists := profile.Permissions()["bash"]; exists {
		t.Error("Permissions() exposed mutable profile state")
	}
	if profile.Permission("bash") != agentprofile.Deny {
		t.Errorf("Permission(omitted bash) = %q, want deny", profile.Permission("bash"))
	}

	digest := sha256.Sum256(content)
	if profile.ContentSHA256() != digest || profile.ContentHash() != hex.EncodeToString(digest[:]) {
		t.Errorf("content hashes = (%x, %q), want %x", profile.ContentSHA256(), profile.ContentHash(), digest)
	}
	wantJSON := `{"instructions":"Implement the Work Item carefully.\n","model":"openai/gpt-5.2","name":"developer","path":".omnigrex/team/developer.md","permissions":{"edit":"deny","read":"allow"},"role":"DEVELOPER","runtime":"opencode-acp/v1","steps":250,"variant":"high"}`
	if got := string(profile.CanonicalJSON()); got != wantJSON {
		t.Errorf("CanonicalJSON() = %s\nwant = %s", got, wantJSON)
	}
	if !json.Valid(profile.CanonicalJSON()) {
		t.Error("CanonicalJSON() is not valid JSON")
	}
	canonical := profile.CanonicalJSON()
	canonical[0] = '['
	if profile.CanonicalJSON()[0] != '{' {
		t.Error("CanonicalJSON() exposed mutable profile state")
	}
}

func TestParseInfersReviewerIdentityAndAcceptsEveryKnownTool(t *testing.T) {
	content := []byte("---\nruntime: opencode-acp/v1\nmodel: anthropic/claude-sonnet-4\nsteps: 1000\npermissions:\n  read: allow\n  edit: deny\n  glob: allow\n  grep: allow\n  list: deny\n  patch: deny\n  bash: deny\n  task: allow\n  webfetch: allow\n  websearch: deny\n  codesearch: allow\n  todoread: allow\n  todowrite: deny\n  question: allow\n  skill: allow\n---\nReview the Change Proposal.\n")

	profile, err := agentprofile.Parse(agentprofile.Reviewer, content)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if profile.Name() != agentprofile.Reviewer || profile.Role() != agentprofile.RoleReviewer || profile.Path() != ".omnigrex/team/reviewer.md" {
		t.Errorf("reviewer identity = (%q, %q, %q)", profile.Name(), profile.Role(), profile.Path())
	}
	if profile.Variant() != "" || len(profile.Permissions()) != 15 {
		t.Errorf("reviewer optional values = (%q, %#v)", profile.Variant(), profile.Permissions())
	}
}

func TestRepositoryAgentProfilesAreValid(t *testing.T) {
	for _, name := range []agentprofile.Name{agentprofile.Developer, agentprofile.Reviewer} {
		path := "../../.omnigrex/team/" + string(name) + ".md"
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		parsed, err := agentprofile.Parse(name, content)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if parsed.Runtime() != "opencode-acp/v1" || parsed.Model() != "opencode-go/muse-spark-1.3-contributor" {
			t.Errorf("%s runtime/model = (%q, %q)", path, parsed.Runtime(), parsed.Model())
		}
		if name == agentprofile.Reviewer && (parsed.Permission("edit") != agentprofile.Deny || parsed.Permission("patch") != agentprofile.Deny) {
			t.Errorf("%s permits OpenCode editing tools", path)
		}
	}
}

func TestParseAcceptsExactDelimitersWithCRLFLineEndings(t *testing.T) {
	content := []byte("---\r\nruntime: opencode-acp/v1\r\nmodel: openai/gpt-5.2\r\nsteps: 10\r\npermissions:\r\n  read: allow\r\n---\r\nInstructions.\r\n")

	profile, err := agentprofile.Parse(agentprofile.Developer, content)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if profile.Instructions() != "Instructions.\r\n" {
		t.Errorf("Instructions() = %q", profile.Instructions())
	}
}

func TestParseRejectsInvalidProfiles(t *testing.T) {
	valid := "---\nruntime: opencode-acp/v1\nmodel: openai/gpt-5.2\nsteps: 10\npermissions:\n  read: allow\n---\nInstructions.\n"
	tests := []struct {
		name    string
		content []byte
	}{
		{name: "unknown requested profile", content: []byte(valid)},
		{name: "missing opening delimiter", content: []byte(valid[4:])},
		{name: "opening delimiter suffix", content: []byte("--- \nruntime: opencode-acp/v1\nmodel: openai/gpt-5.2\nsteps: 10\npermissions: {read: allow}\n---\nInstructions.\n")},
		{name: "malformed closing delimiter", content: []byte("---\nruntime: opencode-acp/v1\nmodel: openai/gpt-5.2\nsteps: 10\npermissions:\n  read: allow\n----\nInstructions.\n")},
		{name: "BOM before delimiter", content: append([]byte{0xef, 0xbb, 0xbf}, []byte(valid)...)},
		{name: "BOM in instructions", content: append([]byte(valid), 0xef, 0xbb, 0xbf)},
		{name: "unknown key", content: []byte("---\nruntime: opencode-acp/v1\nmodel: openai/gpt-5.2\nsteps: 10\npermissions: {read: allow}\nrole: ADMIN\n---\nInstructions.\n")},
		{name: "duplicate key", content: []byte("---\nruntime: opencode-acp/v1\nruntime: other/v2\nmodel: openai/gpt-5.2\nsteps: 10\npermissions: {read: allow}\n---\nInstructions.\n")},
		{name: "alias", content: []byte("---\nruntime: &runtime opencode-acp/v1\nmodel: openai/gpt-5.2\nvariant: *runtime\nsteps: 10\npermissions: {read: allow}\n---\nInstructions.\n")},
		{name: "custom tag", content: []byte("---\nruntime: !profile opencode-acp/v1\nmodel: openai/gpt-5.2\nsteps: 10\npermissions: {read: allow}\n---\nInstructions.\n")},
		{name: "extra YAML document", content: []byte("---\nruntime: opencode-acp/v1\nmodel: openai/gpt-5.2\nsteps: 10\npermissions: {read: allow}\n...\nmodel: other/model\n---\nInstructions.\n")},
		{name: "missing runtime", content: []byte("---\nmodel: openai/gpt-5.2\nsteps: 10\npermissions: {read: allow}\n---\nInstructions.\n")},
		{name: "runtime missing version", content: []byte("---\nruntime: opencode-acp\nmodel: openai/gpt-5.2\nsteps: 10\npermissions: {read: allow}\n---\nInstructions.\n")},
		{name: "runtime extra slash", content: []byte("---\nruntime: organization/opencode-acp/v1\nmodel: openai/gpt-5.2\nsteps: 10\npermissions: {read: allow}\n---\nInstructions.\n")},
		{name: "runtime whitespace", content: []byte("---\nruntime: 'opencode acp/v1'\nmodel: openai/gpt-5.2\nsteps: 10\npermissions: {read: allow}\n---\nInstructions.\n")},
		{name: "model missing provider", content: []byte("---\nruntime: opencode-acp/v1\nmodel: /gpt-5.2\nsteps: 10\npermissions: {read: allow}\n---\nInstructions.\n")},
		{name: "model control character", content: []byte("---\nruntime: opencode-acp/v1\nmodel: \"openai/gpt\\t5.2\"\nsteps: 10\npermissions: {read: allow}\n---\nInstructions.\n")},
		{name: "variant whitespace", content: []byte("---\nruntime: opencode-acp/v1\nmodel: openai/gpt-5.2\nvariant: 'high effort'\nsteps: 10\npermissions: {read: allow}\n---\nInstructions.\n")},
		{name: "zero steps", content: []byte("---\nruntime: opencode-acp/v1\nmodel: openai/gpt-5.2\nsteps: 0\npermissions: {read: allow}\n---\nInstructions.\n")},
		{name: "negative steps", content: []byte("---\nruntime: opencode-acp/v1\nmodel: openai/gpt-5.2\nsteps: -1\npermissions: {read: allow}\n---\nInstructions.\n")},
		{name: "steps over maximum", content: []byte("---\nruntime: opencode-acp/v1\nmodel: openai/gpt-5.2\nsteps: 1001\npermissions: {read: allow}\n---\nInstructions.\n")},
		{name: "empty permissions", content: []byte("---\nruntime: opencode-acp/v1\nmodel: openai/gpt-5.2\nsteps: 10\npermissions: {}\n---\nInstructions.\n")},
		{name: "unknown tool", content: []byte("---\nruntime: opencode-acp/v1\nmodel: openai/gpt-5.2\nsteps: 10\npermissions: {execute: allow}\n---\nInstructions.\n")},
		{name: "unknown action", content: []byte("---\nruntime: opencode-acp/v1\nmodel: openai/gpt-5.2\nsteps: 10\npermissions: {read: ask}\n---\nInstructions.\n")},
		{name: "blank instructions", content: []byte("---\nruntime: opencode-acp/v1\nmodel: openai/gpt-5.2\nsteps: 10\npermissions: {read: allow}\n---\n \t\n")},
		{name: "oversized content", content: bytes.Repeat([]byte("x"), agentprofile.MaxContentSize+1)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requested := agentprofile.Developer
			if test.name == "unknown requested profile" {
				requested = agentprofile.Name("administrator")
			}
			if _, err := agentprofile.Parse(requested, test.content); err == nil {
				t.Fatal("Parse() error = nil, want rejection")
			}
		})
	}
}
