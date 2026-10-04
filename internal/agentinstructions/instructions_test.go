package agentinstructions_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/agentinstructions"
	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestOperatorInstructionsAreLoadedOnceAndScopedByRole(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "common.md", "COMMON_V1")
	write(t, dir, "roles/DEVELOPER.md", "DEVELOPER_ONLY")
	write(t, dir, "roles/REVIEWER.md", "REVIEWER_ONLY")
	policies := role.BuiltinPolicyCatalog()
	first, err := agentinstructions.Load(dir, policies)
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, "common.md", "COMMON_V2")
	second, err := agentinstructions.Load(dir, policies)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := workflow.NewBuiltinDefinition(role.BuiltinCatalog())
	if err != nil {
		t.Fatal(err)
	}
	stage, _ := definition.Stage(workflow.StageImplementation)
	policy, _ := policies.Lookup(role.Developer)
	old := first.Compose(policy, stage, workflow.TurnPurposeRequestedChanges, "PERSONALITY")
	current := second.Compose(policy, stage, workflow.TurnPurposeRequestedChanges, "PERSONALITY")
	if !strings.Contains(old, "COMMON_V1") || strings.Contains(old, "COMMON_V2") || strings.Contains(current, "COMMON_V1") {
		t.Fatal("startup-loaded instructions changed in place or new startup retained old content")
	}
	for _, want := range []string{"COMMON_V2", "DEVELOPER_ONLY", "PERSONALITY", "changes since the previous review", "request_review"} {
		if !strings.Contains(current, want) {
			t.Errorf("current instructions missing %q", want)
		}
	}
	if strings.Contains(current, "REVIEWER_ONLY") || strings.Contains(current, "Current Stage: review") {
		t.Fatal("Developer received another Role/Stage's guidance")
	}
	review, _ := definition.Stage(workflow.StageReview)
	policy, _ = policies.Lookup(role.Reviewer)
	current = second.Compose(policy, review, workflow.TurnPurposeReview, "REVIEW_STYLE")
	for _, want := range []string{"REVIEWER_ONLY", "REVIEW_STYLE", "submit_review.comments", "without intentionally modifying tracked", "discards workspace"} {
		if !strings.Contains(current, want) {
			t.Errorf("Reviewer instructions missing %q", want)
		}
	}
	if strings.Contains(current, "DEVELOPER_ONLY") || strings.Contains(current, "Current Stage: implementation") {
		t.Fatal("Reviewer received another Role/Stage's guidance")
	}
}

func TestOperatorInstructionsOptionalAndInvalidFiles(t *testing.T) {
	policies := role.BuiltinPolicyCatalog()
	if _, err := agentinstructions.Load("", policies); err != nil {
		t.Fatal(err)
	}
	if _, err := agentinstructions.Load(t.TempDir(), policies); err != nil {
		t.Fatal(err)
	}
	if _, err := agentinstructions.Load(filepath.Join(t.TempDir(), "absent"), policies); err == nil {
		t.Fatal("configured missing directory accepted")
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "common.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := agentinstructions.Load(dir, policies); err == nil {
		t.Fatal("directory accepted as an instruction file")
	}
	for _, test := range []struct{ name, path, content string }{
		{"unknown Role", "roles/UNKNOWN.md", "private sentinel"},
		{"wrong extension", "roles/DEVELOPER.txt", "private sentinel"},
		{"unknown root file", "instructions.md", "private sentinel"},
		{"invalid UTF-8", "common.md", "private sentinel\xff"},
		{"NUL", "roles/REVIEWER.md", "private sentinel\x00"},
		{"oversized", "common.md", strings.Repeat("x", (64<<10)+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, test.path, test.content)
			_, err := agentinstructions.Load(dir, policies)
			if err == nil || strings.Contains(err.Error(), "private sentinel") {
				t.Fatalf("invalid instruction diagnostic = %v", err)
			}
		})
	}
}

func TestComposeUsesStageInstructionsRatherThanRoleName(t *testing.T) {
	policy, _ := role.BuiltinPolicyCatalog().Lookup(role.Developer)
	stage := workflow.StageDefinition{ID: "design", Role: role.Developer, Instructions: "DESIGN_ONLY"}
	got := (agentinstructions.Operator{}).Compose(policy, stage, workflow.TurnPurposeInitialDevelopment, "STYLE")
	if !strings.Contains(got, "DESIGN_ONLY") || strings.Contains(got, "merge the default branch") {
		t.Fatalf("custom Stage inherited built-in implementation obligations: %s", got)
	}
}

func TestOperatorInstructionsRejectOversizedCombinedEncodedContent(t *testing.T) {
	for _, test := range []struct{ name, common, role string }{
		{"combined individually valid files", strings.Repeat("c", 40<<10), strings.Repeat("r", 40<<10)},
		{"JSON escaping expansion", strings.Repeat("<", 12000), ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, "common.md", test.common)
			write(t, dir, "roles/REVIEWER.md", test.role)
			_, err := agentinstructions.Load(dir, role.BuiltinPolicyCatalog())
			if err == nil || !strings.Contains(err.Error(), "JSON-encoded budget") {
				t.Fatalf("unsafe combined instructions accepted: %v", err)
			}
		})
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
