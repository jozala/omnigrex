package role

import (
	"errors"
	"fmt"
	"strings"
)

var ErrInvalidPolicyCatalog = errors.New("invalid Role Policy Catalog")

// CredentialAuthority identifies the repository identity used for an operation.
// It is deliberately independent from the Role performing that operation.
type CredentialAuthority string

const (
	OrchestratorAuthority CredentialAuthority = "ORCHESTRATOR"
	ReviewerAuthority     CredentialAuthority = "REVIEWER"
)

// TrustedToolsRevisionPolicy selects which repository revision may supply executable tool configuration.
type TrustedToolsRevisionPolicy string

const (
	TurnRevisionTrustedTools  TrustedToolsRevisionPolicy = "TURN_REVISION"
	DefaultBranchTrustedTools TrustedToolsRevisionPolicy = "DEFAULT_BRANCH"
)

// OpenCodePolicy owns Role-specific adapter hardening independently from mutable Agent Profile permissions.
type OpenCodePolicy struct {
	HardenProjectConfiguration bool
	AllowFileEdits             bool
}

// Policy owns capability and isolation decisions for one canonical Role.
type Policy struct {
	Role                          ID
	MCPTools                      []string
	RequiresChangeProposal        bool
	RepositoryCredentialAuthority CredentialAuthority
	ToolCredentialAuthorities     map[string]CredentialAuthority
	TrustedToolsRevision          TrustedToolsRevisionPolicy
	OpenCode                      OpenCodePolicy
	DiscardWorkspace              bool
	AllowHumanSessionControl      bool
}

// PolicyCatalog is an immutable set of policies whose keys exactly match a Workflow Definition's referenced Roles.
type PolicyCatalog struct {
	ordered []ID
	byRole  map[ID]Policy
}

// NewPolicyCatalog validates and defensively copies policies for exactly the referenced Roles.
func NewPolicyCatalog(referenced []ID, policies []Policy) (PolicyCatalog, error) {
	if len(referenced) == 0 || len(policies) == 0 {
		return PolicyCatalog{}, fmt.Errorf("%w: no referenced Roles or policies", ErrInvalidPolicyCatalog)
	}
	references := make(map[ID]struct{}, len(referenced))
	ordered := make([]ID, 0, len(referenced))
	for _, id := range referenced {
		if !ValidID(id) {
			return PolicyCatalog{}, fmt.Errorf("%w: invalid referenced Role %q", ErrInvalidPolicyCatalog, id)
		}
		if _, duplicate := references[id]; duplicate {
			return PolicyCatalog{}, fmt.Errorf("%w: duplicate referenced Role %q", ErrInvalidPolicyCatalog, id)
		}
		references[id] = struct{}{}
		ordered = append(ordered, id)
	}

	catalog := PolicyCatalog{ordered: ordered, byRole: make(map[ID]Policy, len(policies))}
	for _, policy := range policies {
		if _, referenced := references[policy.Role]; !referenced {
			return PolicyCatalog{}, fmt.Errorf("%w: policy for unreferenced Role %q", ErrInvalidPolicyCatalog, policy.Role)
		}
		if _, duplicate := catalog.byRole[policy.Role]; duplicate {
			return PolicyCatalog{}, fmt.Errorf("%w: duplicate policy for Role %q", ErrInvalidPolicyCatalog, policy.Role)
		}
		if err := validatePolicy(policy); err != nil {
			return PolicyCatalog{}, fmt.Errorf("%w: Role %q: %v", ErrInvalidPolicyCatalog, policy.Role, err)
		}
		catalog.byRole[policy.Role] = clonePolicy(policy)
	}
	for _, id := range ordered {
		if _, ok := catalog.byRole[id]; !ok {
			return PolicyCatalog{}, fmt.Errorf("%w: missing policy for referenced Role %q", ErrInvalidPolicyCatalog, id)
		}
	}
	return catalog, nil
}

// BuiltinPolicyCatalog returns the startup policy for the built-in Workflow Definition.
func BuiltinPolicyCatalog() PolicyCatalog {
	catalog, err := NewBuiltinPolicyCatalog([]ID{Developer, Reviewer})
	if err != nil {
		panic(err)
	}
	return catalog
}

// NewBuiltinPolicyCatalog validates the built-in policies against the Roles referenced by a Workflow Definition.
func NewBuiltinPolicyCatalog(referenced []ID) (PolicyCatalog, error) {
	builtin := map[ID]Policy{
		Developer: {
			Role: Developer,
			MCPTools: []string{
				"get_issue", "list_issue_comments", "get_pull_request", "list_pull_request_reviews", "list_review_threads", "get_check_runs",
				"publish_changes", "open_pr", "request_review", "comment_on_issue", "comment_on_pull_request", "report_blocked",
			},
			RepositoryCredentialAuthority: OrchestratorAuthority,
			TrustedToolsRevision:          TurnRevisionTrustedTools,
			OpenCode:                      OpenCodePolicy{AllowFileEdits: true},
			AllowHumanSessionControl:      true,
		},
		Reviewer: {
			Role: Reviewer,
			MCPTools: []string{
				"get_issue", "list_issue_comments", "get_pull_request", "list_pull_request_reviews", "list_review_threads", "get_check_runs",
				"submit_review", "comment_on_issue", "comment_on_pull_request", "report_blocked",
			},
			RequiresChangeProposal: true, RepositoryCredentialAuthority: OrchestratorAuthority,
			ToolCredentialAuthorities: map[string]CredentialAuthority{"submit_review": ReviewerAuthority},
			TrustedToolsRevision:      DefaultBranchTrustedTools,
			OpenCode:                  OpenCodePolicy{HardenProjectConfiguration: true},
			DiscardWorkspace:          true, AllowHumanSessionControl: true,
		},
	}
	policies := make([]Policy, 0, len(referenced))
	for _, id := range referenced {
		policy, ok := builtin[id]
		if !ok {
			return PolicyCatalog{}, fmt.Errorf("%w: no built-in policy for Role %q", ErrInvalidPolicyCatalog, id)
		}
		policies = append(policies, policy)
	}
	return NewPolicyCatalog(referenced, policies)
}

// Lookup returns a defensive copy of one Role policy.
func (catalog PolicyCatalog) Lookup(id ID) (Policy, bool) {
	policy, ok := catalog.byRole[id]
	if !ok {
		return Policy{}, false
	}
	return clonePolicy(policy), true
}

// Contains reports whether the catalog has a policy for the Role.
func (catalog PolicyCatalog) Contains(id ID) bool {
	_, ok := catalog.byRole[id]
	return ok
}

// Roles returns policy keys in Workflow Definition reference order.
func (catalog PolicyCatalog) Roles() []ID {
	return append([]ID(nil), catalog.ordered...)
}

// CredentialAuthorityForTool returns the exact repository identity for a granted tool.
func (policy Policy) CredentialAuthorityForTool(tool string) (CredentialAuthority, bool) {
	if !containsString(policy.MCPTools, tool) {
		return "", false
	}
	if authority, overridden := policy.ToolCredentialAuthorities[tool]; overridden {
		return authority, true
	}
	return policy.RepositoryCredentialAuthority, true
}

func validatePolicy(policy Policy) error {
	if !ValidID(policy.Role) {
		return errors.New("invalid Role")
	}
	if len(policy.MCPTools) == 0 || policy.RepositoryCredentialAuthority != OrchestratorAuthority ||
		(policy.TrustedToolsRevision != TurnRevisionTrustedTools && policy.TrustedToolsRevision != DefaultBranchTrustedTools) {
		return errors.New("missing capabilities, credential authority, or trusted-tools revision policy")
	}
	seen := make(map[string]struct{}, len(policy.MCPTools))
	for _, tool := range policy.MCPTools {
		if !validToolName(tool) {
			return fmt.Errorf("invalid MCP tool %q", tool)
		}
		if _, duplicate := seen[tool]; duplicate {
			return fmt.Errorf("duplicate MCP tool %q", tool)
		}
		seen[tool] = struct{}{}
		if tool == "submit_review" && policy.ToolCredentialAuthorities[tool] != ReviewerAuthority {
			return errors.New("submit_review must use Reviewer credential authority")
		}
	}
	for tool, authority := range policy.ToolCredentialAuthorities {
		if _, granted := seen[tool]; !granted || !validCredentialAuthority(authority) ||
			authority == ReviewerAuthority && tool != "submit_review" {
			return fmt.Errorf("invalid credential authority override for %q", tool)
		}
	}
	return nil
}

func clonePolicy(policy Policy) Policy {
	policy.MCPTools = append([]string(nil), policy.MCPTools...)
	authorities := policy.ToolCredentialAuthorities
	policy.ToolCredentialAuthorities = make(map[string]CredentialAuthority, len(policy.ToolCredentialAuthorities))
	for tool, authority := range authorities {
		policy.ToolCredentialAuthorities[tool] = authority
	}
	return policy
}

func validCredentialAuthority(authority CredentialAuthority) bool {
	return authority == OrchestratorAuthority || authority == ReviewerAuthority
}

func validToolName(value string) bool {
	if value == "" || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '_') {
			return false
		}
	}
	return true
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
