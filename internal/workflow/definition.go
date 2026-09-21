package workflow

import (
	"errors"
	"fmt"
	"strings"
)

const maxStageIDLength = 64

var ErrInvalidDefinition = errors.New("invalid Workflow Definition")

// StageID is the stable durable identity of an Attempt Stage.
type StageID string

// StageEntry identifies a Stage and the Purpose of the Turn entering it.
type StageEntry struct {
	Stage   StageID
	Purpose TurnPurpose
}

// OutcomeTransition routes one accepted domain outcome from a Stage.
type OutcomeTransition struct {
	Outcome             TurnOutcome
	NextStage           StageID
	NextPurpose         TurnPurpose
	TerminalState       State
	ContinuationStage   StageID
	ConsumesReviewCycle bool
}

// StageDefinition describes one stable position in a Workflow Definition.
type StageDefinition struct {
	ID               StageID
	Role             Role
	State            State
	AcceptedPurposes []TurnPurpose
	Transitions      []OutcomeTransition
	ReviewLimit      uint8
}

// Definition is an immutable graph of Attempt Stages and domain-outcome transitions.
type Definition struct {
	initial StageEntry
	ordered []StageID
	stages  map[StageID]StageDefinition
}

// NewDefinition validates and copies a Workflow Definition.
func NewDefinition(catalog interface{ Contains(Role) bool }, initial StageEntry, stages []StageDefinition) (Definition, error) {
	if catalog == nil || len(stages) == 0 || !validStageID(initial.Stage) || !validTurnPurpose(initial.Purpose) {
		return Definition{}, fmt.Errorf("%w: invalid initial configuration", ErrInvalidDefinition)
	}
	definition := Definition{initial: initial, ordered: make([]StageID, 0, len(stages)), stages: make(map[StageID]StageDefinition, len(stages))}
	for _, stage := range stages {
		// stage validation
		if !validStageID(stage.ID) || !catalog.Contains(stage.Role) ||
			(stage.State != StateDeveloping && stage.State != StateReviewing) || len(stage.AcceptedPurposes) == 0 || len(stage.Transitions) == 0 {
			return Definition{}, fmt.Errorf("%w: invalid Stage %q", ErrInvalidDefinition, stage.ID)
		}
		if _, duplicate := definition.stages[stage.ID]; duplicate {
			return Definition{}, fmt.Errorf("%w: duplicate Stage %q", ErrInvalidDefinition, stage.ID)
		}
		purposeSet := make(map[TurnPurpose]struct{}, len(stage.AcceptedPurposes))
		for _, purpose := range stage.AcceptedPurposes {
			if !validTurnPurpose(purpose) {
				return Definition{}, fmt.Errorf("%w: invalid Purpose for Stage %q", ErrInvalidDefinition, stage.ID)
			}
			if _, duplicate := purposeSet[purpose]; duplicate {
				return Definition{}, fmt.Errorf("%w: duplicate Purpose for Stage %q", ErrInvalidDefinition, stage.ID)
			}
			purposeSet[purpose] = struct{}{}
		}
		if !containsPurpose(stage.AcceptedPurposes, TurnPurposeRetry) ||
			!containsPurpose(stage.AcceptedPurposes, TurnPurposeReactivation) ||
			stage.State == StateReviewing && !containsPurpose(stage.AcceptedPurposes, TurnPurposeSynchronization) {
			return Definition{}, fmt.Errorf("%w: Stage %q omits an operational Purpose", ErrInvalidDefinition, stage.ID)
		}
		outcomeSet := make(map[TurnOutcome]struct{}, len(stage.Transitions))
		for _, transition := range stage.Transitions {
			if !validDefinitionOutcome(transition.Outcome) {
				return Definition{}, fmt.Errorf("%w: invalid outcome for Stage %q", ErrInvalidDefinition, stage.ID)
			}
			if _, duplicate := outcomeSet[transition.Outcome]; duplicate {
				return Definition{}, fmt.Errorf("%w: duplicate outcome for Stage %q", ErrInvalidDefinition, stage.ID)
			}
			outcomeSet[transition.Outcome] = struct{}{}
			successor := transition.NextStage != "" || transition.NextPurpose != ""
			terminal := transition.TerminalState != "" || transition.ContinuationStage != ""
			if successor == terminal || successor && (!validStageID(transition.NextStage) || !validTurnPurpose(transition.NextPurpose)) ||
				terminal && (transition.TerminalState != StatePRReady || !validStageID(transition.ContinuationStage)) ||
				transition.ConsumesReviewCycle && stage.ReviewLimit == 0 {
				return Definition{}, fmt.Errorf("%w: invalid transition for Stage %q", ErrInvalidDefinition, stage.ID)
			}
			switch transition.Outcome {
			case TurnOutcomeChangeProposalReady, TurnOutcomeChangesRequested:
				if !successor {
					return Definition{}, fmt.Errorf("%w: outcome requires a successor for Stage %q", ErrInvalidDefinition, stage.ID)
				}
			case TurnOutcomeApproved:
				if !terminal {
					return Definition{}, fmt.Errorf("%w: approval requires a terminal continuation for Stage %q", ErrInvalidDefinition, stage.ID)
				}
			}
		}

		// add stage to definition
		definition.ordered = append(definition.ordered, stage.ID)
		definition.stages[stage.ID] = cloneStageDefinition(stage)
	}
	// validate initial stage
	initialStage, ok := definition.stages[initial.Stage]
	if !ok || initialStage.State != StateDeveloping || !containsPurpose(initialStage.AcceptedPurposes, initial.Purpose) {
		return Definition{}, fmt.Errorf("%w: initial entry is not accepted", ErrInvalidDefinition)
	}
	// validate relationships between stages
	for _, stage := range definition.stages {
		for _, transition := range stage.Transitions {
			if transition.NextStage != "" {
				target, ok := definition.stages[transition.NextStage]
				if !ok || !containsPurpose(target.AcceptedPurposes, transition.NextPurpose) {
					return Definition{}, fmt.Errorf("%w: invalid successor from Stage %q", ErrInvalidDefinition, stage.ID)
				}
			}
			if transition.ContinuationStage != "" {
				if _, ok := definition.stages[transition.ContinuationStage]; !ok {
					return Definition{}, fmt.Errorf("%w: invalid continuation from Stage %q", ErrInvalidDefinition, stage.ID)
				}
			}
		}
	}
	return definition, nil
}

// InitialEntry returns the first normal Stage entry for a new Workflow.
func (definition Definition) InitialEntry() StageEntry {
	return definition.initial
}

// Stage returns a defensive copy of one Stage definition.
func (definition Definition) Stage(id StageID) (StageDefinition, bool) {
	stage, ok := definition.stages[id]
	if !ok {
		return StageDefinition{}, false
	}
	return cloneStageDefinition(stage), true
}

// Transition returns the transition for an accepted Stage outcome.
func (definition Definition) Transition(stageID StageID, outcome TurnOutcome) (OutcomeTransition, bool) {
	stage, ok := definition.stages[stageID]
	if !ok {
		return OutcomeTransition{}, false
	}
	for _, transition := range stage.Transitions {
		if transition.Outcome == outcome {
			return transition, true
		}
	}
	return OutcomeTransition{}, false
}

// ExpectedOutcomes returns normal successful outcomes in declaration order.
func (definition Definition) ExpectedOutcomes(stageID StageID) []TurnOutcome {
	stage, ok := definition.stages[stageID]
	if !ok {
		return nil
	}
	outcomes := make([]TurnOutcome, len(stage.Transitions))
	for index, transition := range stage.Transitions {
		outcomes[index] = transition.Outcome
	}
	return outcomes
}

// StageIDs returns Stage IDs in declaration order.
func (definition Definition) StageIDs() []StageID {
	return append([]StageID(nil), definition.ordered...)
}

// Roles returns unique referenced Roles in Stage declaration order.
func (definition Definition) Roles() []Role {
	roles := make([]Role, 0, len(definition.ordered))
	seen := make(map[Role]struct{}, len(definition.ordered))
	for _, stageID := range definition.ordered {
		role := definition.stages[stageID].Role
		if _, ok := seen[role]; ok {
			continue
		}
		seen[role] = struct{}{}
		roles = append(roles, role)
	}
	return roles
}

// ContainsRole reports whether at least one Stage uses the Role.
func (definition Definition) ContainsRole(role Role) bool {
	for _, stage := range definition.stages {
		if stage.Role == role {
			return true
		}
	}
	return false
}

// AcceptsPurpose reports whether the Stage accepts the Turn Purpose.
func (definition Definition) AcceptsPurpose(stageID StageID, purpose TurnPurpose) bool {
	stage, ok := definition.stages[stageID]
	return ok && containsPurpose(stage.AcceptedPurposes, purpose)
}

func cloneStageDefinition(stage StageDefinition) StageDefinition {
	stage.AcceptedPurposes = append([]TurnPurpose(nil), stage.AcceptedPurposes...)
	stage.Transitions = append([]OutcomeTransition(nil), stage.Transitions...)
	return stage
}

func validStageID(id StageID) bool {
	if len(id) == 0 || len(id) > maxStageIDLength || id[0] < 'a' || id[0] > 'z' || strings.TrimSpace(string(id)) != string(id) {
		return false
	}
	for _, character := range id[1:] {
		if character < 'a' || character > 'z' {
			if character < '0' || character > '9' {
				if character != '-' {
					return false
				}
			}
		}
	}
	return true
}

func validTurnPurpose(purpose TurnPurpose) bool {
	switch purpose {
	case TurnPurposeInitialDevelopment, TurnPurposeReview, TurnPurposeRequestedChanges,
		TurnPurposeRetry, TurnPurposeSynchronization, TurnPurposeReactivation:
		return true
	default:
		return false
	}
}

func validDefinitionOutcome(outcome TurnOutcome) bool {
	switch outcome {
	case TurnOutcomeChangeProposalReady, TurnOutcomeChangesRequested, TurnOutcomeApproved:
		return true
	default:
		return false
	}
}

func containsPurpose(purposes []TurnPurpose, purpose TurnPurpose) bool {
	for _, candidate := range purposes {
		if candidate == purpose {
			return true
		}
	}
	return false
}
