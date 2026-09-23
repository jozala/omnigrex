package mcp_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestCapabilitiesForRoleReturnsDeterministicCredentialFreeNames(t *testing.T) {
	wantDeveloper := []string{
		"get_issue", "list_issue_comments", "get_pull_request", "list_pull_request_reviews", "list_review_threads", "get_check_runs",
		"publish_changes", "open_pr", "request_review", "comment_on_issue", "comment_on_pull_request", "report_blocked",
	}
	wantReviewer := []string{
		"get_issue", "list_issue_comments", "get_pull_request", "list_pull_request_reviews", "list_review_threads", "get_check_runs",
		"submit_review", "comment_on_issue", "comment_on_pull_request", "report_blocked",
	}
	for _, test := range []struct {
		role workflow.Role
		want []string
	}{
		{role: workflow.RoleDeveloper, want: wantDeveloper},
		{role: workflow.RoleReviewer, want: wantReviewer},
	} {
		got, err := mcp.CapabilitiesForRole(test.role)
		if err != nil || !slices.Equal(got, test.want) {
			t.Fatalf("CapabilitiesForRole(%q) = (%v, %v), want %v", test.role, got, err, test.want)
		}
		got[0] = "modified"
		second, err := mcp.CapabilitiesForRole(test.role)
		if err != nil || !slices.Equal(second, test.want) {
			t.Fatalf("second CapabilitiesForRole(%q) = (%v, %v), want defensive deterministic copy", test.role, second, err)
		}
	}
}

func TestCapabilitiesForRoleRejectsUnknownRole(t *testing.T) {
	capabilities, err := mcp.CapabilitiesForRole(workflow.Role("OPERATOR"))
	if !errors.Is(err, mcp.ErrInvalidConfiguration) || capabilities != nil {
		t.Fatalf("CapabilitiesForRole() = (%v, %v), want ErrInvalidConfiguration", capabilities, err)
	}
}
