package agentprofile_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/role"
)

func TestValidThinkingAcceptsNativeMaxVocabulary(t *testing.T) {
	for _, level := range []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"} {
		if !agentprofile.ValidThinking(level) {
			t.Errorf("ValidThinking(%q) = false, want true", level)
		}
	}
	for _, level := range []string{"", " ", "MAX", "XHigh", "x-high", "impossible", "ultra"} {
		if agentprofile.ValidThinking(level) {
			t.Errorf("ValidThinking(%q) = true, want false", level)
		}
	}
}

func TestPiThinkingMaxSurvivesParseCanonicalAndSnapshot(t *testing.T) {
	base := "---\nname: developer\nrole: DEVELOPER\nruntime: pi-rpc/v2\nmodel: openai/o4-mini\nthinking: high\npermissions:\n  read: allow\n---\nImplement carefully.\n"
	content := []byte(strings.Replace(base, "thinking: high\n", "thinking: max\n", 1))
	p, err := agentprofile.Parse(".omnigrex/team/developer.md", content, role.BuiltinPolicyCatalog())
	if err != nil {
		t.Fatalf("Parse(max) error = %v", err)
	}
	if p.Thinking() != "max" {
		t.Fatalf("Thinking() = %q, want max", p.Thinking())
	}
	if !strings.Contains(string(p.CanonicalJSON()), `"thinking":"max"`) {
		t.Fatalf("canonical JSON does not preserve max: %s", p.CanonicalJSON())
	}
	decoded, err := agentprofile.DecodeSnapshot(p.CanonicalJSON(), role.BuiltinPolicyCatalog())
	if err != nil {
		t.Fatalf("DecodeSnapshot(max) error = %v", err)
	}
	if decoded.Thinking() != "max" || string(decoded.CanonicalJSON()) != string(p.CanonicalJSON()) {
		t.Fatalf("snapshot round trip changed max: %q %s", decoded.Thinking(), decoded.CanonicalJSON())
	}
}

func TestOwnedSessionValidatorMatchesGoThinkingVocabulary(t *testing.T) {
	root := findRepositoryRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "agent", "pi", "session-validation.mjs"))
	if err != nil {
		t.Fatalf("read owned Session validator: %v", err)
	}
	text := string(content)
	for _, level := range []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"} {
		if !strings.Contains(text, "'"+level+"'") {
			t.Errorf("owned Session validator does not accept %q", level)
		}
		if !agentprofile.ValidThinking(level) {
			t.Errorf("Go validator does not accept %q", level)
		}
	}
	if strings.Contains(text, "'ultra'") || strings.Contains(text, "'impossible'") {
		t.Error("owned Session validator accepts an unknown thinking level")
	}
}

func TestPiThinkingVocabularyRoundTripsWithoutAliasing(t *testing.T) {
	base := "---\nname: developer\nrole: DEVELOPER\nruntime: pi-rpc/v2\nmodel: openai/o4-mini\nthinking: high\npermissions:\n  read: allow\n---\nImplement carefully.\n"
	for _, level := range []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"} {
		content := []byte(strings.Replace(base, "thinking: high\n", "thinking: "+level+"\n", 1))
		p, err := agentprofile.Parse(".omnigrex/team/developer.md", content, role.BuiltinPolicyCatalog())
		if err != nil {
			t.Fatalf("Parse(%q) error = %v", level, err)
		}
		if p.Thinking() != level {
			t.Fatalf("Thinking() = %q, want %q", p.Thinking(), level)
		}
		decoded, err := agentprofile.DecodeSnapshot(p.CanonicalJSON(), role.BuiltinPolicyCatalog())
		if err != nil {
			t.Fatalf("DecodeSnapshot(%q) error = %v", level, err)
		}
		if decoded.Thinking() != level {
			t.Fatalf("snapshot Thinking() = %q, want %q", decoded.Thinking(), level)
		}
		if level == "max" && (decoded.Thinking() == "xhigh" || decoded.Thinking() == "high") {
			t.Fatalf("max was aliased to %q", decoded.Thinking())
		}
		if level == "xhigh" && decoded.Thinking() == "max" {
			t.Fatalf("xhigh was aliased to max")
		}
	}
}
