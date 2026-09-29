package agentturn

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

const EventEnvelopeSchemaVersion = 2

var ErrInvalidEventEnvelope = errors.New("invalid Agent Turn event envelope")

type triggeringEvent struct {
	Stage       workflow.StageID     `json:"stage"`
	Role        workflow.Role        `json:"role"`
	TurnPurpose workflow.TurnPurpose `json:"turn_purpose"`
}

type eventEnvelope struct {
	SchemaVersion        int                    `json:"schema_version"`
	WorkflowID           string                 `json:"workflow_id"`
	IssueNumber          int64                  `json:"issue_number"`
	RepositoryID         int64                  `json:"repository_id"`
	AgentParticipantID   string                 `json:"agent_participant_id"`
	AssignmentGeneration int                    `json:"assignment_generation"`
	AgentSessionID       string                 `json:"agent_session_id"`
	AgentTurnID          string                 `json:"agent_turn_id"`
	TriggeringEvent      triggeringEvent        `json:"triggering_event"`
	PullRequestNumber    *int64                 `json:"pull_request_number,omitempty"`
	CurrentHeadSHA       string                 `json:"current_head_sha"`
	ExpectedHeadSHA      string                 `json:"expected_head_sha"`
	ExpectedOutcomes     []workflow.TurnOutcome `json:"expected_outcomes"`
	AllowedOutcomes      []workflow.TurnOutcome `json:"allowed_outcomes"`
	MCPCapabilities      []string               `json:"mcp_capabilities"`
}

// BuildEventEnvelope projects a Turn using the deployment's Workflow Definition and Role policies.
func BuildEventEnvelope(execution store.AgentTurnExecutionContext, currentHeadSHA string, definition workflow.Definition, policies role.PolicyCatalog) ([]acp.ContentBlock, error) {
	policy, ok := policies.Lookup(execution.Assignment.Role)
	if !ok {
		return nil, fmt.Errorf("%w: Role policy", ErrInvalidEventEnvelope)
	}
	if err := validateEventEnvelopeContext(execution, currentHeadSHA, definition, policy); err != nil {
		return nil, err
	}

	expectedOutcomes := definition.ExpectedOutcomes(execution.Turn.Stage)
	allowedOutcomes := append(append([]workflow.TurnOutcome(nil), expectedOutcomes...), workflow.TurnOutcomeBlocked)
	capabilities := append([]string(nil), policy.MCPTools...)
	envelope := eventEnvelope{
		SchemaVersion: EventEnvelopeSchemaVersion,
		WorkflowID:    execution.WorkflowID, IssueNumber: execution.Issue.Number, RepositoryID: execution.Repository.ID,
		AgentParticipantID: execution.Assignment.ID, AssignmentGeneration: execution.Assignment.Generation,
		AgentSessionID: execution.Session.ID, AgentTurnID: execution.Turn.ID,
		TriggeringEvent: triggeringEvent{Stage: execution.Turn.Stage, Role: execution.Assignment.Role, TurnPurpose: execution.Turn.Purpose},
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

func validateEventEnvelopeContext(execution store.AgentTurnExecutionContext, currentHeadSHA string, definition workflow.Definition, policy role.Policy) error {
	assignment := execution.Assignment
	session := execution.Session
	turn := execution.Turn
	stage, stageExists := definition.Stage(turn.Stage)
	if !validEnvelopeString(execution.WorkflowID) || execution.Repository.ID <= 0 || execution.Issue.ID <= 0 || execution.Issue.Number <= 0 ||
		!validEnvelopeString(assignment.ID) || assignment.WorkflowID != execution.WorkflowID || assignment.Generation <= 0 ||
		!validEnvelopeString(session.ID) || session.AgentAssignmentID != assignment.ID ||
		!validEnvelopeString(turn.ID) || turn.AgentAssignmentID != assignment.ID || turn.AgentSessionID != session.ID ||
		!validEnvelopeString(currentHeadSHA) || policy.Role != assignment.Role || !stageExists || stage.Role != assignment.Role || !definition.AcceptsPurpose(turn.Stage, turn.Purpose) {
		return ErrInvalidEventEnvelope
	}
	if execution.ChangeProposal == nil {
		if turn.ChangeProposalID != "" || turn.ExpectedHeadSHA != "" || policy.RequiresChangeProposal ||
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
