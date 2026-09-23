package agentprofile_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/role"
)

func TestParseReturnsFrontmatterIdentityIndependentOfPath(t *testing.T) {
	content := []byte("---\nname: release_developer\nrole: DEVELOPER\nruntime: opencode-acp/v1\nmodel: openai/gpt-5.2\nvariant: high\nsteps: 250\npermissions:\n  read: allow\n  edit: deny\n---\nImplement the Work Item carefully.\n")

	profile, err := agentprofile.Parse(".omnigrex/team/arbitrary.. profile.md", content, role.BuiltinPolicyCatalog())
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if profile.Name() != "release_developer" || profile.Role() != role.Developer || profile.Path() != ".omnigrex/team/arbitrary.. profile.md" {
		t.Errorf("identity = (%q, %q, %q)", profile.Name(), profile.Role(), profile.Path())
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
	if _, exists := profile.Permissions()["bash"]; exists || profile.Permission("bash") != agentprofile.Deny {
		t.Error("Profile exposed mutable permissions or did not deny an omitted tool")
	}

	digest := sha256.Sum256(content)
	if profile.ContentSHA256() != digest || profile.ContentHash() != hex.EncodeToString(digest[:]) {
		t.Errorf("content hashes = (%x, %q), want %x", profile.ContentSHA256(), profile.ContentHash(), digest)
	}
	wantJSON := `{"instructions":"Implement the Work Item carefully.\n","model":"openai/gpt-5.2","name":"release_developer","path":".omnigrex/team/arbitrary.. profile.md","permissions":{"edit":"deny","read":"allow"},"role":"DEVELOPER","runtime":"opencode-acp/v1","steps":250,"variant":"high"}`
	if got := string(profile.CanonicalJSON()); got != wantJSON || !json.Valid(profile.CanonicalJSON()) {
		t.Errorf("CanonicalJSON() = %s\nwant = %s", got, wantJSON)
	}
	canonical := profile.CanonicalJSON()
	canonical[0] = '['
	if profile.CanonicalJSON()[0] != '{' {
		t.Error("CanonicalJSON() exposed mutable profile state")
	}
}

func TestParseRequiresExplicitNameAndPolicyBackedRole(t *testing.T) {
	valid := validProfileContent("developer", role.Developer, "openai/gpt-5.2", "Instructions.\n")
	tests := []struct {
		name    string
		content []byte
	}{
		{name: "missing name", content: []byte(strings.Replace(string(valid), "name: developer\n", "", 1))},
		{name: "missing Role", content: []byte(strings.Replace(string(valid), "role: DEVELOPER\n", "", 1))},
		{name: "unsafe uppercase name", content: []byte(strings.Replace(string(valid), "name: developer", "name: Developer", 1))},
		{name: "unsafe spaced name", content: []byte(strings.Replace(string(valid), "name: developer", "name: 'developer profile'", 1))},
		{name: "Role without policy", content: []byte(strings.Replace(string(valid), "role: DEVELOPER", "role: ARCHITECT", 1))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := agentprofile.Parse(".omnigrex/team/developer.md", test.content, role.BuiltinPolicyCatalog()); !errorsIsInvalidProfile(err) {
				t.Fatalf("Parse() error = %v, want ErrInvalidProfile", err)
			}
		})
	}
}

func TestParseUsesInjectedPolicyCatalog(t *testing.T) {
	const architect role.ID = "ARCHITECT"
	policies, err := role.NewPolicyCatalog([]role.ID{architect}, []role.Policy{{
		Role: architect, MCPTools: []string{"get_issue"},
		RepositoryCredentialAuthority: role.OrchestratorAuthority,
		TrustedToolsRevision:          role.TurnRevisionTrustedTools,
	}})
	if err != nil {
		t.Fatal(err)
	}
	content := validProfileContent("solution-architect", architect, "openai/gpt-5.2", "Design the solution.\n")
	profile, err := agentprofile.Parse(".omnigrex/team/architecture.md", content, policies)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if profile.Name() != "solution-architect" || profile.Role() != architect {
		t.Errorf("identity = (%q, %q)", profile.Name(), profile.Role())
	}
}

func TestRepositoryAgentProfilesAreValid(t *testing.T) {
	for _, path := range []string{"../../.omnigrex/team/developer.md", "../../.omnigrex/team/reviewer.md"} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		profile, err := agentprofile.Parse(".omnigrex/team/"+strings.TrimPrefix(path, "../../.omnigrex/team/"), content, role.BuiltinPolicyCatalog())
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if profile.Runtime() != "opencode-acp/v1" || profile.Model() != "opencode-go/muse-spark-1.3-contributor" {
			t.Errorf("%s runtime/model = (%q, %q)", path, profile.Runtime(), profile.Model())
		}
	}
}

func TestParseAcceptsEveryKnownToolForReviewer(t *testing.T) {
	content := []byte("---\nname: quality\nrole: REVIEWER\nruntime: opencode-acp/v1\nmodel: anthropic/claude-sonnet-4\nsteps: 1000\npermissions:\n  read: allow\n  edit: deny\n  glob: allow\n  grep: allow\n  list: deny\n  patch: deny\n  bash: deny\n  task: allow\n  webfetch: allow\n  websearch: deny\n  codesearch: allow\n  todoread: allow\n  todowrite: deny\n  question: allow\n  skill: allow\n---\nReview the Change Proposal.\n")

	profile, err := agentprofile.Parse(".omnigrex/team/review.md", content, role.BuiltinPolicyCatalog())
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if profile.Name() != "quality" || profile.Role() != role.Reviewer || profile.Variant() != "" || len(profile.Permissions()) != 15 {
		t.Errorf("Profile = (%q, %q, %q, %#v)", profile.Name(), profile.Role(), profile.Variant(), profile.Permissions())
	}
}

func TestParseAcceptsExactDelimitersWithCRLFLineEndings(t *testing.T) {
	content := []byte("---\r\nname: developer\r\nrole: DEVELOPER\r\nruntime: opencode-acp/v1\r\nmodel: openai/gpt-5.2\r\nsteps: 10\r\npermissions:\r\n  read: allow\r\n---\r\nInstructions.\r\n")
	profile, err := agentprofile.Parse(".omnigrex/team/developer.md", content, role.BuiltinPolicyCatalog())
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if profile.Instructions() != "Instructions.\r\n" {
		t.Errorf("Instructions() = %q", profile.Instructions())
	}
}

func TestParseRejectsUnsafePaths(t *testing.T) {
	content := validProfileContent("developer", role.Developer, "openai/gpt-5.2", "Instructions.\n")
	for _, path := range []string{
		"developer.md",
		".omnigrex/team.md",
		".omnigrex/team/developer.MD",
		".omnigrex/team/nested/developer.md",
		".omnigrex/team/../developer.md",
		".omnigrex/team/.md",
		".omnigrex/team/developer.md ",
	} {
		if _, err := agentprofile.Parse(path, content, role.BuiltinPolicyCatalog()); !errorsIsInvalidProfile(err) {
			t.Errorf("Parse(%q) error = %v, want ErrInvalidProfile", path, err)
		}
	}
}

func TestParseRejectsReviewerEditingPermissions(t *testing.T) {
	for _, tool := range []string{"edit", "patch"} {
		t.Run(tool, func(t *testing.T) {
			content := []byte("---\nname: reviewer\nrole: REVIEWER\nruntime: opencode-acp/v1\nmodel: openai/gpt-5.2\nsteps: 10\npermissions:\n  " + tool + ": allow\n---\nReview the Change Proposal.\n")
			if _, err := agentprofile.Parse(".omnigrex/team/reviewer.md", content, role.BuiltinPolicyCatalog()); !errorsIsInvalidProfile(err) {
				t.Fatalf("Parse() error = %v, want editing permission rejection", err)
			}
		})
	}
}

func TestParseRejectsInvalidProfileConfiguration(t *testing.T) {
	valid := string(validProfileContent("developer", role.Developer, "openai/gpt-5.2", "Instructions.\n"))
	tests := []struct {
		name    string
		content []byte
	}{
		{name: "missing opening delimiter", content: []byte(valid[4:])},
		{name: "opening delimiter suffix", content: []byte(strings.Replace(valid, "---\n", "--- \n", 1))},
		{name: "malformed closing delimiter", content: []byte(strings.Replace(valid, "---\nInstructions", "----\nInstructions", 1))},
		{name: "BOM before delimiter", content: append([]byte{0xef, 0xbb, 0xbf}, []byte(valid)...)},
		{name: "BOM in instructions", content: append([]byte(valid), 0xef, 0xbb, 0xbf)},
		{name: "unknown key", content: []byte(strings.Replace(valid, "runtime:", "extra: value\nruntime:", 1))},
		{name: "duplicate key", content: []byte(strings.Replace(valid, "runtime:", "runtime: other/v2\nruntime:", 1))},
		{name: "alias", content: []byte(strings.Replace(valid, "runtime: opencode-acp/v1", "runtime: &runtime opencode-acp/v1\nvariant: *runtime", 1))},
		{name: "custom tag", content: []byte(strings.Replace(valid, "runtime: opencode-acp/v1", "runtime: !profile opencode-acp/v1", 1))},
		{name: "missing runtime", content: []byte(strings.Replace(valid, "runtime: opencode-acp/v1\n", "", 1))},
		{name: "runtime missing version", content: []byte(strings.Replace(valid, "opencode-acp/v1", "opencode-acp", 1))},
		{name: "runtime extra slash", content: []byte(strings.Replace(valid, "opencode-acp/v1", "org/opencode/v1", 1))},
		{name: "model missing provider", content: []byte(strings.Replace(valid, "openai/gpt-5.2", "/gpt-5.2", 1))},
		{name: "zero steps", content: []byte(strings.Replace(valid, "steps: 50", "steps: 0", 1))},
		{name: "steps over maximum", content: []byte(strings.Replace(valid, "steps: 50", "steps: 1001", 1))},
		{name: "empty permissions", content: []byte(strings.Replace(valid, "permissions:\n  read: allow", "permissions: {}", 1))},
		{name: "unknown tool", content: []byte(strings.Replace(valid, "read: allow", "execute: allow", 1))},
		{name: "unknown action", content: []byte(strings.Replace(valid, "read: allow", "read: ask", 1))},
		{name: "blank instructions", content: []byte(strings.Replace(valid, "Instructions.\n", " \t\n", 1))},
		{name: "oversized content", content: bytes.Repeat([]byte("x"), agentprofile.MaxContentSize+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := agentprofile.Parse(".omnigrex/team/developer.md", test.content, role.BuiltinPolicyCatalog()); err == nil {
				t.Fatal("Parse() error = nil, want rejection")
			}
		})
	}
}

func validProfileContent(name string, roleID role.ID, model, instructions string) []byte {
	return []byte("---\nname: " + name + "\nrole: " + string(roleID) + "\nruntime: opencode-acp/v1\nmodel: " + model + "\nsteps: 50\npermissions:\n  read: allow\n---\n" + instructions)
}

func errorsIsInvalidProfile(err error) bool {
	return errors.Is(err, agentprofile.ErrInvalidProfile)
}
