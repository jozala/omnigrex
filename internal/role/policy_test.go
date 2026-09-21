package role_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/jozala/omnigrex/internal/role"
)

func TestPolicyCatalogValidatesExactWorkflowRoleSet(t *testing.T) {
	developer := testPolicy(role.Developer)
	reviewer := testPolicy(role.Reviewer)
	tests := []struct {
		name       string
		referenced []role.ID
		policies   []role.Policy
	}{
		{name: "missing referenced policy", referenced: []role.ID{role.Developer, role.Reviewer}, policies: []role.Policy{developer}},
		{name: "unreferenced policy", referenced: []role.ID{role.Developer}, policies: []role.Policy{developer, reviewer}},
		{name: "duplicate policy", referenced: []role.ID{role.Developer}, policies: []role.Policy{developer, developer}},
		{name: "duplicate reference", referenced: []role.ID{role.Developer, role.Developer}, policies: []role.Policy{developer}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := role.NewPolicyCatalog(test.referenced, test.policies); !errors.Is(err, role.ErrInvalidPolicyCatalog) {
				t.Fatalf("NewPolicyCatalog() error = %v, want ErrInvalidPolicyCatalog", err)
			}
		})
	}
}

func TestPolicyCatalogRejectsInvalidPolicyDetails(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*role.Policy)
	}{
		{name: "invalid Role", mutate: func(policy *role.Policy) { policy.Role = "developer" }},
		{name: "no tools", mutate: func(policy *role.Policy) { policy.MCPTools = nil }},
		{name: "duplicate tool", mutate: func(policy *role.Policy) { policy.MCPTools = []string{"get_issue", "get_issue"} }},
		{name: "unknown authority", mutate: func(policy *role.Policy) { policy.RepositoryCredentialAuthority = "DEVELOPER" }},
		{name: "Reviewer repository authority", mutate: func(policy *role.Policy) { policy.RepositoryCredentialAuthority = role.ReviewerAuthority }},
		{name: "override for ungranted tool", mutate: func(policy *role.Policy) {
			policy.ToolCredentialAuthorities = map[string]role.CredentialAuthority{"submit_review": role.ReviewerAuthority}
		}},
		{name: "Reviewer authority for another tool", mutate: func(policy *role.Policy) {
			policy.ToolCredentialAuthorities = map[string]role.CredentialAuthority{"get_issue": role.ReviewerAuthority}
		}},
		{name: "submit review without Reviewer authority", mutate: func(policy *role.Policy) {
			policy.MCPTools = append(policy.MCPTools, "submit_review")
		}},
		{name: "trusted tools revision", mutate: func(policy *role.Policy) { policy.TrustedToolsRevision = "FEATURE_BRANCH" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := testPolicy(role.Developer)
			test.mutate(&policy)
			if _, err := role.NewPolicyCatalog([]role.ID{role.Developer}, []role.Policy{policy}); !errors.Is(err, role.ErrInvalidPolicyCatalog) {
				t.Fatalf("NewPolicyCatalog() error = %v, want ErrInvalidPolicyCatalog", err)
			}
		})
	}
}

func TestPolicyCatalogDefensiveCopiesMutableValues(t *testing.T) {
	policy := testPolicy(role.Developer)
	policy.MCPTools = append(policy.MCPTools, "submit_review")
	policy.ToolCredentialAuthorities = map[string]role.CredentialAuthority{"submit_review": role.ReviewerAuthority}
	catalog, err := role.NewPolicyCatalog([]role.ID{role.Developer}, []role.Policy{policy})
	if err != nil {
		t.Fatal(err)
	}

	policy.MCPTools[0] = "mutated"
	policy.ToolCredentialAuthorities["submit_review"] = role.OrchestratorAuthority
	first, ok := catalog.Lookup(role.Developer)
	if !ok {
		t.Fatal("Lookup() did not find Developer")
	}
	first.MCPTools[0] = "also_mutated"
	first.ToolCredentialAuthorities["submit_review"] = role.OrchestratorAuthority
	roles := catalog.Roles()
	roles[0] = role.Reviewer

	got, ok := catalog.Lookup(role.Developer)
	if !ok || !reflect.DeepEqual(got.MCPTools, []string{"get_issue", "submit_review"}) || got.ToolCredentialAuthorities["submit_review"] != role.ReviewerAuthority ||
		!reflect.DeepEqual(catalog.Roles(), []role.ID{role.Developer}) {
		t.Fatalf("catalog was mutated: %#v, Roles %v", got, catalog.Roles())
	}
}

func TestBuiltinPolicySeparatesRoleFromCredentialAuthority(t *testing.T) {
	catalog := role.BuiltinPolicyCatalog()
	reviewer, ok := catalog.Lookup(role.Reviewer)
	if !ok {
		t.Fatal("Reviewer policy is missing")
	}
	for _, tool := range []string{"get_issue", "comment_on_pull_request"} {
		if authority, granted := reviewer.CredentialAuthorityForTool(tool); !granted || authority != role.OrchestratorAuthority {
			t.Errorf("Reviewer %s authority = (%q, %t), want Orchestrator", tool, authority, granted)
		}
	}
	if authority, granted := reviewer.CredentialAuthorityForTool("submit_review"); !granted || authority != role.ReviewerAuthority {
		t.Errorf("Reviewer submit_review authority = (%q, %t), want Reviewer", authority, granted)
	}
	if _, granted := reviewer.CredentialAuthorityForTool("publish_changes"); granted {
		t.Fatal("Reviewer unexpectedly granted publish_changes")
	}
}

func TestBuiltinPolicyCatalogSupportsReferencedSubset(t *testing.T) {
	catalog, err := role.NewBuiltinPolicyCatalog([]role.ID{role.Developer})
	if err != nil {
		t.Fatalf("NewBuiltinPolicyCatalog() error = %v", err)
	}
	if !reflect.DeepEqual(catalog.Roles(), []role.ID{role.Developer}) || catalog.Contains(role.Reviewer) {
		t.Fatalf("subset catalog Roles = %v", catalog.Roles())
	}
}

func testPolicy(id role.ID) role.Policy {
	return role.Policy{
		Role:                          id,
		MCPTools:                      []string{"get_issue"},
		RepositoryCredentialAuthority: role.OrchestratorAuthority,
		TrustedToolsRevision:          role.TurnRevisionTrustedTools,
	}
}
