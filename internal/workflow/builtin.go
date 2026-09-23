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
			AcceptedPurposes: []TurnPurpose{
				TurnPurposeInitialDevelopment, TurnPurposeRequestedChanges, TurnPurposeRetry, TurnPurposeReactivation,
			},
			Transitions: []OutcomeTransition{{
				Outcome: TurnOutcomeChangeProposalReady, NextStage: StageReview, NextPurpose: TurnPurposeReview,
			}},
		},
		{
			ID: StageReview, Role: role.Reviewer, State: StateReviewing,
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
