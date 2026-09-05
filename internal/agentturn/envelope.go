package agentturn

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

const EventEnvelopeSchemaVersion = 1

var ErrInvalidEventEnvelope = errors.New("invalid Agent Turn event envelope")

type triggeringEvent struct {
	Role        workflow.Role        `json:"role"`
	TurnPurpose workflow.TurnPurpose `json:"turn_purpose"`
}

type eventEnvelope struct {
	SchemaVersion     int                    `json:"schema_version"`
	WorkflowID        string                 `json:"workflow_id"`
	IssueNumber       int64                  `json:"issue_number"`
	RepositoryID      int64                  `json:"repository_id"`
	AgentAssignmentID string                 `json:"agent_assignment_id"`
	AgentSessionID    string                 `json:"agent_session_id"`
	AgentTurnID       string                 `json:"agent_turn_id"`
	TriggeringEvent   triggeringEvent        `json:"triggering_event"`
	PullRequestNumber *int64                 `json:"pull_request_number,omitempty"`
	CurrentHeadSHA    string                 `json:"current_head_sha"`
	ExpectedHeadSHA   string                 `json:"expected_head_sha"`
	ExpectedOutcomes  []workflow.TurnOutcome `json:"expected_outcomes"`
	AllowedOutcomes   []workflow.TurnOutcome `json:"allowed_outcomes"`
	MCPCapabilities   []string               `json:"mcp_capabilities"`
}

// BuildEventEnvelope projects durable Agent Turn context into one canonical JSON ACP text block.
func BuildEventEnvelope(execution store.AgentTurnExecutionContext, currentHeadSHA string) ([]acp.ContentBlock, error) {
	if err := validateEventEnvelopeContext(execution, currentHeadSHA); err != nil {
		return nil, err
	}

	expectedOutcomes, allowedOutcomes := eventEnvelopeOutcomes(execution.Assignment.Role)
	capabilities, err := mcp.CapabilitiesForRole(execution.Assignment.Role)
	if err != nil {
		return nil, fmt.Errorf("%w: capabilities", ErrInvalidEventEnvelope)
	}
	envelope := eventEnvelope{
		SchemaVersion: EventEnvelopeSchemaVersion,
		WorkflowID:    execution.WorkflowID, IssueNumber: execution.Issue.Number, RepositoryID: execution.Repository.ID,
		AgentAssignmentID: execution.Assignment.ID, AgentSessionID: execution.Session.ID, AgentTurnID: execution.Turn.ID,
		TriggeringEvent: triggeringEvent{Role: execution.Assignment.Role, TurnPurpose: execution.Turn.Purpose},
		CurrentHeadSHA:  currentHeadSHA, ExpectedHeadSHA: execution.Turn.ExpectedHeadSHA,
		ExpectedOutcomes: expectedOutcomes, AllowedOutcomes: allowedOutcomes, MCPCapabilities: capabilities,
	}
	if execution.ChangeProposal != nil {
		number := execution.ChangeProposal.PullRequestNumber
		envelope.PullRequestNumber = &number
	}

	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encode Agent Turn event envelope: %w", err)
	}
	return []acp.ContentBlock{acp.TextContent(string(encoded))}, nil
}

func validateEventEnvelopeContext(execution store.AgentTurnExecutionContext, currentHeadSHA string) error {
	assignment := execution.Assignment
	session := execution.Session
	turn := execution.Turn
	if !validEnvelopeString(execution.WorkflowID) || execution.Repository.ID <= 0 || execution.Issue.ID <= 0 || execution.Issue.Number <= 0 ||
		!validEnvelopeString(assignment.ID) || assignment.WorkflowID != execution.WorkflowID ||
		!validEnvelopeString(session.ID) || session.AgentAssignmentID != assignment.ID ||
		!validEnvelopeString(turn.ID) || turn.AgentAssignmentID != assignment.ID || turn.AgentSessionID != session.ID ||
		!validEnvelopeString(currentHeadSHA) || !validEnvelopePurpose(assignment.Role, turn.Purpose) {
		return ErrInvalidEventEnvelope
	}
	if execution.ChangeProposal == nil {
		if turn.ChangeProposalID != "" || turn.ExpectedHeadSHA != "" || assignment.Role == workflow.RoleReviewer ||
			turn.Purpose == workflow.TurnPurposeRequestedChanges {
			return ErrInvalidEventEnvelope
		}
		return nil
	}
	proposal := execution.ChangeProposal
	if turn.Purpose == workflow.TurnPurposeInitialDevelopment || !validEnvelopeString(proposal.ID) || proposal.PullRequestNumber <= 0 ||
		!validEnvelopeString(proposal.HeadSHA) || turn.ChangeProposalID != proposal.ID || turn.ExpectedHeadSHA != proposal.HeadSHA {
		return ErrInvalidEventEnvelope
	}
	return nil
}

func validEnvelopePurpose(role workflow.Role, purpose workflow.TurnPurpose) bool {
	switch role {
	case workflow.RoleDeveloper:
		switch purpose {
		case workflow.TurnPurposeInitialDevelopment, workflow.TurnPurposeRequestedChanges,
			workflow.TurnPurposeRetry, workflow.TurnPurposeReactivation:
			return true
		}
	case workflow.RoleReviewer:
		switch purpose {
		case workflow.TurnPurposeReview, workflow.TurnPurposeRetry,
			workflow.TurnPurposeSynchronization, workflow.TurnPurposeReactivation:
			return true
		}
	}
	return false
}

func validEnvelopeString(value string) bool {
	if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func eventEnvelopeOutcomes(role workflow.Role) ([]workflow.TurnOutcome, []workflow.TurnOutcome) {
	if role == workflow.RoleDeveloper {
		return []workflow.TurnOutcome{workflow.TurnOutcomeChangeProposalReady}, []workflow.TurnOutcome{
			workflow.TurnOutcomeChangeProposalReady, workflow.TurnOutcomeBlocked,
		}
	}
	return []workflow.TurnOutcome{workflow.TurnOutcomeApproved, workflow.TurnOutcomeChangesRequested}, []workflow.TurnOutcome{
		workflow.TurnOutcomeApproved, workflow.TurnOutcomeChangesRequested, workflow.TurnOutcomeBlocked,
	}
}
