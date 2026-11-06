package collaboration

import (
	"reflect"
	"slices"
	"strings"
)

// Assessment is an explicit agent interpretation, never a semantic inference by
// admission. Evidence and corrections refer to immutable accepted input.
type Assessment struct {
	OriginOperationID      string   `json:"origin_operation_id,omitempty"`
	InputID                string   `json:"input_id"`
	EvidenceIDs            []string `json:"evidence_ids"`
	Disposition            string   `json:"disposition"`
	Explanation            string   `json:"explanation"`
	Corrects               []string `json:"corrects,omitempty"`
	InvalidateConfirmation bool     `json:"invalidate_confirmation,omitempty"`
}

func (s *State) latestAssessment(id string) *Assessment {
	for i := len(s.Assessments) - 1; i >= 0; i-- {
		if s.Assessments[i].InputID == id {
			return &s.Assessments[i]
		}
	}
	return nil
}

func (s *State) HasAssessmentEffect(operation string, wanted []Assessment) bool {
	if operation == "" || len(wanted) == 0 {
		return false
	}
	for _, a := range wanted {
		a.OriginOperationID = operation
		if !slices.ContainsFunc(s.Assessments, func(actual Assessment) bool { return reflect.DeepEqual(actual, a) }) {
			return false
		}
	}
	return true
}

// Assess is atomic: an invalid batch cannot leave partial corrections or clear
// an earlier material conflict. The caller binds operation to its reservation.
func (s *State) Assess(operation, participant, turn string, batch []Assessment) error {
	if operation == "" || len(batch) == 0 || !slices.ContainsFunc(s.Preparations, func(p Preparation) bool { return p.ParticipantID == participant && p.TurnID == turn }) {
		return ErrInvalid
	}
	delivered := func(id string) bool {
		return slices.ContainsFunc(s.Inputs, func(in Input) bool {
			return in.ID == id && in.ParticipantID == participant && in.DeliveredTo != "" && !in.DeliveryPending
		})
	}
	seen := map[string]bool{}
	for _, a := range batch {
		if seen[a.InputID] || !delivered(a.InputID) || strings.TrimSpace(a.Explanation) == "" || len(a.EvidenceIDs) == 0 || !slices.Contains(a.EvidenceIDs, a.InputID) {
			return ErrInvalid
		}
		seen[a.InputID] = true
		if a.Disposition != "changed" && a.Disposition != "unchanged" && a.Disposition != "uncertain" && a.Disposition != "confirmed" {
			return ErrInvalid
		}
		for _, id := range a.EvidenceIDs {
			if !delivered(id) {
				return ErrInvalid
			}
		}
		for _, id := range a.Corrects {
			if id == a.InputID || !delivered(id) {
				return ErrInvalid
			}
		}
		if a.Disposition == "confirmed" && !slices.ContainsFunc(s.Inputs, func(in Input) bool { return in.ID == a.InputID && in.Kind == "confirm-plan" }) {
			return ErrInvalid
		}
	}
	for _, a := range batch {
		a.OriginOperationID = operation
		a.EvidenceIDs = slices.Clone(a.EvidenceIDs)
		a.Corrects = slices.Clone(a.Corrects)
		s.Assessments = append(s.Assessments, a)
		// Withdrawal or material uncertainty never erases history, nor does resolving
		// it resurrect the authority of an already invalidated confirmation.
		invalidate := a.InvalidateConfirmation
		for _, in := range s.Inputs {
			if in.ID == a.InputID && in.Kind == "confirm-plan" && a.Disposition != "confirmed" {
				invalidate = true
			}
		}
		if invalidate && s.Planning != nil && s.Planning.ReadyRevision == "" {
			for _, in := range s.Inputs {
				if in.Kind == "confirm-plan" && !slices.Contains(s.Planning.InvalidatedConfirmations, in.ID) {
					s.Planning.InvalidatedConfirmations = append(s.Planning.InvalidatedConfirmations, in.ID)
				}
			}
		}
	}
	return nil
}

func (s *State) AssessmentBlocked(participant string) bool {
	for _, in := range s.Inputs {
		if in.ParticipantID != participant {
			continue
		}
		a := s.latestAssessment(in.ID)
		if in.DeliveredTo == "" || in.DeliveryPending || a == nil || a.Disposition == "uncertain" {
			return true
		}
	}
	return false
}

func (s *State) explicitlyCorrects(id, prior string) bool {
	return slices.ContainsFunc(s.Assessments, func(a Assessment) bool {
		return a.InputID == id && a.Disposition == "changed" && slices.Contains(a.Corrects, prior)
	})
}
