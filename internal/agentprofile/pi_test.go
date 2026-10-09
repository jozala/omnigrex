package agentprofile_test

import (
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/role"
)

func TestPiProfileUsesNativeConfiguration(t *testing.T) {
	content := "---\nname: developer\nrole: DEVELOPER\nruntime: pi-rpc/v1\nmodel: openai/o4-mini\nthinking: high\npermissions:\n  read: allow\n  write: allow\n  edit: allow\n  find: allow\n  ls: allow\n  skill: allow\n---\nImplement carefully.\n"
	p, err := agentprofile.Parse(".omnigrex/team/developer.md", []byte(content), role.BuiltinPolicyCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if p.Thinking() != "high" || strings.Contains(string(p.CanonicalJSON()), "steps") || strings.Contains(string(p.CanonicalJSON()), "variant") {
		t.Fatalf("Pi configuration: %s", p.CanonicalJSON())
	}
	if _, err := agentprofile.Parse(".omnigrex/team/developer.md", []byte(strings.Replace(content, "openai/o4-mini", "openrouter/openai/o4-mini", 1)), role.BuiltinPolicyCatalog()); err != nil {
		t.Fatalf("explicit model ID containing a slash: %v", err)
	}
	for _, change := range []string{"steps: 10\n", "steps: 0\n", "variant: high\n", "variant: ''\n", "thinking: impossible\n"} {
		invalid := strings.Replace(content, "thinking: high\n", change, 1)
		if _, err := agentprofile.Parse(".omnigrex/team/developer.md", []byte(invalid), role.BuiltinPolicyCatalog()); err == nil {
			t.Fatalf("accepted %q", change)
		}
	}
	for _, tool := range []string{"task", "patch", "webfetch", "glob", "todoread"} {
		invalid := strings.Replace(content, "  read: allow", "  "+tool+": allow", 1)
		if _, err := agentprofile.Parse(".omnigrex/team/developer.md", []byte(invalid), role.BuiltinPolicyCatalog()); err == nil {
			t.Fatalf("accepted unavailable Pi tool %q", tool)
		}
	}
	for _, tool := range []string{"write", "edit"} {
		invalid := strings.ReplaceAll(content, "DEVELOPER", "REVIEWER")
		invalid = strings.ReplaceAll(invalid, "  write: allow\n", "")
		invalid = strings.ReplaceAll(invalid, "  edit: allow\n", "")
		invalid = strings.Replace(invalid, "permissions:\n", "permissions:\n  "+tool+": allow\n", 1)
		if _, err := agentprofile.Parse(".omnigrex/team/developer.md", []byte(invalid), role.BuiltinPolicyCatalog()); err == nil {
			t.Fatalf("Reviewer allowed %q", tool)
		}
	}
}
