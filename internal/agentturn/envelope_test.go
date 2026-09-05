package agentturn_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/agentturn"
	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestBuildEventEnvelopeReturnsOneCanonicalDeveloperTextBlock(t *testing.T) {
	execution := envelopeExecutionContext(workflow.RoleDeveloper, workflow.TurnPurposeInitialDevelopment, nil)
	execution.Turn.AgentProfileConfig = json.RawMessage(`{"instructions":"sensitive-body-sentinel sensitive-comment-sentinel sensitive-review-sentinel sensitive-check-sentinel sensitive-finding-sentinel sensitive-credential-sentinel sensitive-model-sentinel"}`)

	content, err := agentturn.BuildEventEnvelope(execution, "1111111111111111111111111111111111111111")
	if err != nil {
		t.Fatalf("BuildEventEnvelope() error = %v", err)
	}
	if len(content) != 1 || content[0].Type != "text" || content[0].Data != "" || content[0].MIMEType != "" || content[0].URI != "" {
		t.Fatalf("BuildEventEnvelope() content = %#v, want one text block", content)
	}
	const want = `{"schema_version":1,"workflow_id":"40000000-0000-4000-8000-000000000001","issue_number":17,"repository_id":41,"agent_assignment_id":"10000000-0000-4000-8000-000000000001","agent_session_id":"20000000-0000-4000-8000-000000000001","agent_turn_id":"30000000-0000-4000-8000-000000000001","triggering_event":{"role":"DEVELOPER","turn_purpose":"INITIAL_DEVELOPMENT"},"current_head_sha":"1111111111111111111111111111111111111111","expected_head_sha":"","expected_outcomes":["CHANGE_PROPOSAL_READY"],"allowed_outcomes":["CHANGE_PROPOSAL_READY","BLOCKED"],"mcp_capabilities":["get_issue","list_issue_comments","get_pull_request","list_pull_request_reviews","list_review_threads","get_check_runs","publish_changes","open_pr","request_review","comment_on_issue","comment_on_pull_request","report_blocked"]}`
	if content[0] != acp.TextContent(want) {
		t.Fatalf("BuildEventEnvelope() text = %s\nwant = %s", content[0].Text, want)
	}
	second, err := agentturn.BuildEventEnvelope(execution, "1111111111111111111111111111111111111111")
	if err != nil || !reflect.DeepEqual(second, content) {
		t.Fatalf("second BuildEventEnvelope() = (%#v, %v), want deterministic output", second, err)
	}
	for _, forbidden := range []string{"sensitive-body-sentinel", "sensitive-comment-sentinel", "sensitive-review-sentinel", "sensitive-check-sentinel", "sensitive-finding-sentinel", "sensitive-credential-sentinel", "sensitive-model-sentinel"} {
		if strings.Contains(content[0].Text, forbidden) {
			t.Errorf("event envelope contains forbidden source text %q", forbidden)
		}
	}
}

func TestBuildEventEnvelopeIncludesReviewerPullRequestAndRoleOutcomes(t *testing.T) {
	proposal := &store.AgentTurnChangeProposal{
		ID: "60000000-0000-4000-8000-000000000001", PullRequestNumber: 23,
		HeadSHA: "2222222222222222222222222222222222222222",
	}
	execution := envelopeExecutionContext(workflow.RoleReviewer, workflow.TurnPurposeReview, proposal)

	content, err := agentturn.BuildEventEnvelope(execution, "3333333333333333333333333333333333333333")
	if err != nil {
		t.Fatalf("BuildEventEnvelope() error = %v", err)
	}
	const want = `{"schema_version":1,"workflow_id":"40000000-0000-4000-8000-000000000001","issue_number":17,"repository_id":41,"agent_assignment_id":"10000000-0000-4000-8000-000000000001","agent_session_id":"20000000-0000-4000-8000-000000000001","agent_turn_id":"30000000-0000-4000-8000-000000000001","triggering_event":{"role":"REVIEWER","turn_purpose":"REVIEW"},"pull_request_number":23,"current_head_sha":"3333333333333333333333333333333333333333","expected_head_sha":"2222222222222222222222222222222222222222","expected_outcomes":["APPROVED","CHANGES_REQUESTED"],"allowed_outcomes":["APPROVED","CHANGES_REQUESTED","BLOCKED"],"mcp_capabilities":["get_issue","list_issue_comments","get_pull_request","list_pull_request_reviews","list_review_threads","get_check_runs","submit_review","comment_on_issue","comment_on_pull_request","report_blocked"]}`
	if len(content) != 1 || content[0] != acp.TextContent(want) {
		t.Fatalf("BuildEventEnvelope() = %#v\nwant text = %s", content, want)
	}
}

func TestBuildEventEnvelopeSupportsEveryDurableTurnTrigger(t *testing.T) {
	proposal := &store.AgentTurnChangeProposal{
		ID: "60000000-0000-4000-8000-000000000001", PullRequestNumber: 23,
		HeadSHA: "2222222222222222222222222222222222222222",
	}
	for _, test := range []struct {
		name     string
		role     workflow.Role
		purpose  workflow.TurnPurpose
		proposal *store.AgentTurnChangeProposal
	}{
		{name: "initial development", role: workflow.RoleDeveloper, purpose: workflow.TurnPurposeInitialDevelopment},
		{name: "requested changes", role: workflow.RoleDeveloper, purpose: workflow.TurnPurposeRequestedChanges, proposal: proposal},
		{name: "review", role: workflow.RoleReviewer, purpose: workflow.TurnPurposeReview, proposal: proposal},
		{name: "Developer retry", role: workflow.RoleDeveloper, purpose: workflow.TurnPurposeRetry},
		{name: "Reviewer retry", role: workflow.RoleReviewer, purpose: workflow.TurnPurposeRetry, proposal: proposal},
		{name: "Developer reactivation", role: workflow.RoleDeveloper, purpose: workflow.TurnPurposeReactivation},
		{name: "Reviewer reactivation", role: workflow.RoleReviewer, purpose: workflow.TurnPurposeReactivation, proposal: proposal},
		{name: "Reviewer synchronization", role: workflow.RoleReviewer, purpose: workflow.TurnPurposeSynchronization, proposal: proposal},
	} {
		t.Run(test.name, func(t *testing.T) {
			content, err := agentturn.BuildEventEnvelope(envelopeExecutionContext(test.role, test.purpose, test.proposal), "1111111111111111111111111111111111111111")
			if err != nil || len(content) != 1 {
				t.Fatalf("BuildEventEnvelope() = (%#v, %v)", content, err)
			}
			var envelope struct {
				TriggeringEvent struct {
					Role        workflow.Role        `json:"role"`
					TurnPurpose workflow.TurnPurpose `json:"turn_purpose"`
				} `json:"triggering_event"`
			}
			if err := json.Unmarshal([]byte(content[0].Text), &envelope); err != nil {
				t.Fatalf("decode event envelope: %v", err)
			}
			if envelope.TriggeringEvent.Role != test.role || envelope.TriggeringEvent.TurnPurpose != test.purpose {
				t.Fatalf("triggering event = %#v, want %s/%s", envelope.TriggeringEvent, test.role, test.purpose)
			}
		})
	}
}

func TestBuildEventEnvelopeRejectsInvalidDurableContext(t *testing.T) {
	proposal := &store.AgentTurnChangeProposal{
		ID: "60000000-0000-4000-8000-000000000001", PullRequestNumber: 23,
		HeadSHA: "2222222222222222222222222222222222222222",
	}
	tests := []struct {
		name       string
		execution  store.AgentTurnExecutionContext
		currentSHA string
	}{
		{name: "missing workflow", execution: func() store.AgentTurnExecutionContext {
			value := envelopeExecutionContext(workflow.RoleDeveloper, workflow.TurnPurposeInitialDevelopment, nil)
			value.WorkflowID = ""
			return value
		}(), currentSHA: "head"},
		{name: "missing Issue number", execution: func() store.AgentTurnExecutionContext {
			value := envelopeExecutionContext(workflow.RoleDeveloper, workflow.TurnPurposeInitialDevelopment, nil)
			value.Issue.Number = 0
			return value
		}(), currentSHA: "head"},
		{name: "mismatched assignment", execution: func() store.AgentTurnExecutionContext {
			value := envelopeExecutionContext(workflow.RoleDeveloper, workflow.TurnPurposeInitialDevelopment, nil)
			value.Session.AgentAssignmentID = "different"
			return value
		}(), currentSHA: "head"},
		{name: "blank current head", execution: envelopeExecutionContext(workflow.RoleDeveloper, workflow.TurnPurposeInitialDevelopment, nil)},
		{name: "Reviewer without proposal", execution: envelopeExecutionContext(workflow.RoleReviewer, workflow.TurnPurposeReview, nil), currentSHA: "head"},
		{name: "requested changes without proposal", execution: envelopeExecutionContext(workflow.RoleDeveloper, workflow.TurnPurposeRequestedChanges, nil), currentSHA: "head"},
		{name: "initial development with proposal", execution: envelopeExecutionContext(workflow.RoleDeveloper, workflow.TurnPurposeInitialDevelopment, proposal), currentSHA: "head"},
		{name: "Developer review purpose", execution: envelopeExecutionContext(workflow.RoleDeveloper, workflow.TurnPurposeReview, proposal), currentSHA: "head"},
		{name: "Reviewer requested changes purpose", execution: envelopeExecutionContext(workflow.RoleReviewer, workflow.TurnPurposeRequestedChanges, proposal), currentSHA: "head"},
		{name: "proposal without expected head", execution: func() store.AgentTurnExecutionContext {
			value := envelopeExecutionContext(workflow.RoleReviewer, workflow.TurnPurposeReview, proposal)
			value.Turn.ExpectedHeadSHA = ""
			return value
		}(), currentSHA: "head"},
		{name: "proposal expected head mismatch", execution: func() store.AgentTurnExecutionContext {
			value := envelopeExecutionContext(workflow.RoleReviewer, workflow.TurnPurposeReview, proposal)
			value.Turn.ExpectedHeadSHA = "different"
			return value
		}(), currentSHA: "head"},
		{name: "expected head without proposal", execution: func() store.AgentTurnExecutionContext {
			value := envelopeExecutionContext(workflow.RoleDeveloper, workflow.TurnPurposeInitialDevelopment, nil)
			value.Turn.ExpectedHeadSHA = "unexpected"
			return value
		}(), currentSHA: "head"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content, err := agentturn.BuildEventEnvelope(test.execution, test.currentSHA)
			if !errors.Is(err, agentturn.ErrInvalidEventEnvelope) || content != nil {
				t.Fatalf("BuildEventEnvelope() = (%#v, %v), want ErrInvalidEventEnvelope", content, err)
			}
		})
	}
}

func envelopeExecutionContext(role workflow.Role, purpose workflow.TurnPurpose, proposal *store.AgentTurnChangeProposal) store.AgentTurnExecutionContext {
	execution := store.AgentTurnExecutionContext{
		WorkflowID: "40000000-0000-4000-8000-000000000001",
		Repository: store.AgentTurnRepository{ID: 41, Owner: "owner-body", Name: "repository-comment"},
		Issue:      store.AgentTurnIssue{ID: 51, Number: 17},
		Assignment: store.AgentAssignment{ID: "10000000-0000-4000-8000-000000000001", WorkflowID: "40000000-0000-4000-8000-000000000001", Role: role},
		Session:    store.AgentSession{ID: "20000000-0000-4000-8000-000000000001", AgentAssignmentID: "10000000-0000-4000-8000-000000000001"},
		Turn: store.AgentTurn{
			ID: "30000000-0000-4000-8000-000000000001", AgentAssignmentID: "10000000-0000-4000-8000-000000000001",
			AgentTurnSpec: store.AgentTurnSpec{AgentSessionID: "20000000-0000-4000-8000-000000000001", Purpose: purpose},
		},
		ChangeProposal: proposal,
	}
	if proposal != nil {
		execution.Turn.ChangeProposalID = proposal.ID
		execution.Turn.ExpectedHeadSHA = proposal.HeadSHA
	}
	return execution
}
