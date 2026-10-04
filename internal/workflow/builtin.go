package workflow

import "github.com/jozala/omnigrex/internal/role"

const (
	StageImplementation             StageID = "implementation"
	StageReview                     StageID = "review"
	BuiltinInfrastructureRetryLimit uint8   = 1
)

// NewBuiltinDefinition constructs the deployment-wide initial Workflow Definition.
func NewBuiltinDefinition(catalog role.Catalog) (Definition, error) {
	return NewDefinition(catalog, StageEntry{Stage: StageImplementation, Purpose: TurnPurposeInitialDevelopment}, []StageDefinition{
		{
			ID: StageImplementation, Role: role.Developer, State: StateDeveloping,
			Instructions: implementationInstructions,
			PurposeInstructions: map[TurnPurpose]string{
				TurnPurposeInitialDevelopment: "Implement the Work Item and create its initial Change Proposal.",
				TurnPurposeRequestedChanges:   "Inspect all substantive review findings before revising the existing Change Proposal; explain the changes since the previous review in the new handoff summary.",
			},
			AcceptedPurposes: []TurnPurpose{
				TurnPurposeInitialDevelopment, TurnPurposeRequestedChanges, TurnPurposeRetry, TurnPurposeReactivation,
			},
			Transitions: []OutcomeTransition{{
				Outcome: TurnOutcomeChangeProposalReady, NextStage: StageReview, NextPurpose: TurnPurposeReview,
			}},
		},
		{
			ID: StageReview, Role: role.Reviewer, State: StateReviewing,
			Instructions: reviewInstructions,
			PurposeInstructions: map[TurnPurpose]string{
				TurnPurposeSynchronization: "The Change Proposal head changed; evaluate the current head and recheck earlier findings rather than carrying forward a stale verdict.",
			},
			AcceptedPurposes: []TurnPurpose{
				TurnPurposeReview, TurnPurposeRetry, TurnPurposeSynchronization, TurnPurposeReactivation,
			},
			ReviewLimit: 3,
			Transitions: []OutcomeTransition{
				{
					Outcome: TurnOutcomeChangesRequested, NextStage: StageImplementation, NextPurpose: TurnPurposeRequestedChanges,
					ConsumesReviewCycle: true,
				},
				{
					Outcome: TurnOutcomeApproved, TerminalState: StatePRReady, ContinuationStage: StageReview,
					ConsumesReviewCycle: true,
				},
			},
		},
	})
}
