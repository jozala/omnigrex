package role_test

import (
	"github.com/jozala/omnigrex/internal/role"
	"testing"
)

func TestPlannerPolicyAllowsInvestigationButNotImplementation(t *testing.T) {
	metadata, ok := role.BuiltinCatalog().Lookup(role.Planner)
	if !ok || metadata.DisplayName != "Planner" {
		t.Fatal("Planner identity missing")
	}
	catalog, err := role.NewBuiltinPolicyCatalog([]role.ID{role.Planner})
	if err != nil {
		t.Fatal(err)
	}
	policy, ok := catalog.Lookup(role.Planner)
	if !ok || !policy.DiscardWorkspace || !policy.AllowFileEdits || !policy.AllowHumanSessionControl || policy.RequiresChangeProposal {
		t.Fatalf("incorrect investigation policy: %#v", policy)
	}
	for _, tool := range []string{"get_issue", "list_issue_comments", "refresh_default_branch", "request_clarification", "resolve_clarification", "get_collaboration_record", "report_blocked"} {
		authority, granted := policy.CredentialAuthorityForTool(tool)
		if !granted || authority != role.OrchestratorAuthority {
			t.Errorf("missing investigation capability %s", tool)
		}
	}
	for _, tool := range []string{"publish_changes", "open_pr", "request_review", "submit_review", "comment_on_pull_request", "create_issue", "merge"} {
		if _, granted := policy.CredentialAuthorityForTool(tool); granted {
			t.Errorf("Planner granted %s", tool)
		}
	}
	if !role.BuiltinPolicyCatalog().Contains(role.Planner) {
		t.Fatal("Planner policy unavailable")
	}
}
