package collaboration

import (
	"slices"
	"time"
)

// ImplementationAction preserves human intent independently of confirmation.
// No timer grants, expires, or renews this authority.
type ImplementationAction struct {
	SourceSequence uint64    `json:"source_sequence,omitempty"`
	AcceptedAt     time.Time `json:"accepted_at"`
	ID             string    `json:"id"`
	RevisionID     string    `json:"revision_id"`
	Kind           string    `json:"kind"`
	Actor          Actor     `json:"actor"`
	Source         Source    `json:"source"`
}

type ImplementationBinding struct {
	CycleID         string `json:"cycle_id,omitempty"`
	TurnID          string `json:"turn_id"`
	RevisionID      string `json:"revision_id"`
	AuthorizationID string `json:"authorization_id"`
}
type ImplementationState struct {
	Bindings             []ImplementationBinding `json:"bindings,omitempty"`
	Actions              []ImplementationAction  `json:"actions"`
	Invalidated          []string                `json:"invalidated,omitempty"`
	FirstDeveloperTurnID string                  `json:"first_developer_turn_id,omitempty"`
	AcceptedRevisionID   string                  `json:"accepted_revision_id,omitempty"`
	AuthorizationID      string                  `json:"authorization_id,omitempty"`
}

func exactConfirmation(kind string) bool { return kind == "confirm-plan" || kind == "implement" }

// ImplementationGrant is unused, current human authority, not an admission.
func (s *State) ImplementationGrant() *ImplementationAction {
	if s.Planning == nil || s.Planning.Implementation.FirstDeveloperTurnID != "" && s.ActiveReplanning() == nil {
		return nil
	}
	p := s.Planning
	if len(p.Implementation.Actions) == 0 {
		return nil
	}
	a := p.Implementation.Actions[len(p.Implementation.Actions)-1]
	if a.Kind != "implement" || a.RevisionID != p.CurrentRevision || slices.Contains(p.Implementation.Invalidated, a.ID) || slices.Contains(p.InvalidatedConfirmations, a.ID) {
		return nil
	}
	return &a
}

// ImplementationValidated requires a current explicit Planner validation of all
// known input. Publication and exact-pair consent are necessary, never sufficient.
func (s *State) ImplementationValidated() bool {
	grant := s.ImplementationGrant()
	r := s.CurrentCheckpoint()
	if grant == nil || r == nil || !r.Published || r.Source == nil || r.Content.Status != "proposed" || s.Planning.ReadyRevision != r.ID {
		return false
	}
	a := s.latestAssessment(grant.ID)
	if a == nil || a.Disposition != "confirmed" {
		return false
	}
	for i := len(s.Planning.Revalidations) - 1; i >= 0; i-- {
		v := s.Planning.Revalidations[i]
		if v.RevisionID == r.ID && s.PlanningValidationCurrent(v) && slices.Contains(v.ConsideredInputIDs, grant.ID) {
			return true
		}
	}
	return false
}

func (s *State) ImplementationCommit() string {
	if !s.ImplementationValidated() {
		return ""
	}
	for i := len(s.Planning.Revalidations) - 1; i >= 0; i-- {
		v := s.Planning.Revalidations[i]
		if s.PlanningValidationCurrent(v) {
			return v.RepositoryCommit
		}
	}
	return ""
}

func (s *State) implementationTarget(target, participant string) string {
	r := s.CurrentCheckpoint()
	if s.Planning != nil && s.Planning.Implementation.FirstDeveloperTurnID != "" && s.ActiveReplanning() == nil {
		return "Implementation has already started. Withdrawal cannot stop it; close the original Work Item Issue to use the existing stopping mechanism."
	}
	if r == nil || !r.Published || r.Source == nil || r.Content.Status != "proposed" || r.ID != target || r.ParticipantID != participant {
		return "Use the current published proposed pair's exact short reference, for example `/omnigrex implement P2`. Draft, superseded and unrelated pairs cannot authorize implementation."
	}
	return ""
}

// Source sequence, when comparable, is human creation order. Webhook receipt
// timestamps cannot override it (including retries admitted out of arrival order).
// Channels without a comparable sequence conservatively use receipt order.
func (s *State) implementationBeforeLatestAction(a Admission) bool {
	for _, event := range s.Planning.Implementation.Actions {
		if a.Source.Channel == event.Source.Channel && a.SourceSequence > 0 && event.SourceSequence > 0 {
			if a.SourceSequence <= event.SourceSequence {
				return true
			}
			continue
		}
		if !a.At.After(event.AcceptedAt) {
			return true
		}
	}
	return false
}
