package collaboration

import "slices"

// StaleDecision contains scoped structural references only, never input bodies
// or interpretation text. RoundReference locates the historical record to read;
// QuestionID is the immutable question identity required by Resolve.
type StaleDecision struct {
	RoundReference       string   `json:"round_reference"`
	QuestionID           string   `json:"question_id"`
	EvidenceReferences   []string `json:"evidence_references"`
	CorrectionReferences []string `json:"correction_references"`
}

// StaleDecisions uses the same effective interpretation ordering as Unresolved.
// A correction to one input can stale several decisions, even unchanged ones
// outside the current round. This reports prerequisites, not human semantics.
func (s *State) StaleDecisions(participant string) []StaleDecision {
	effective := s.effectiveResolutions(participant)
	var result []StaleDecision
	seen := map[string]bool{}
	for _, round := range s.Rounds {
		if round.ParticipantID != participant || !round.Published {
			continue
		}
		for _, q := range round.Questions {
			r, ok := effective[q.ID]
			if !ok || seen[q.ID] || s.resolutionCurrent(r) {
				continue
			}
			// Locate the round containing the effective interpretation, not an
			// earlier inherited copy of the same question.
			local := latestResolution(round, q.ID)
			if local == nil || local.Sequence != r.Sequence {
				continue
			}
			seen[q.ID] = true
			d := StaleDecision{RoundReference: round.Reference, QuestionID: q.ID}
			for _, in := range s.Inputs {
				if in.ParticipantID != participant {
					continue
				}
				if slices.Contains(r.ResponseIDs, in.ID) {
					d.EvidenceReferences = append(d.EvidenceReferences, in.Reference)
					continue
				}
				if slices.Contains(r.ConsideredResponseIDs, in.ID) || slices.ContainsFunc(r.ResponseIDs, func(id string) bool { return s.amends(id, in.ID) }) {
					continue
				}
				if slices.ContainsFunc(r.ResponseIDs, func(id string) bool { return s.isAmendmentOfEvidence(in.ID, id) || s.explicitlyCorrects(in.ID, id) }) {
					d.CorrectionReferences = append(d.CorrectionReferences, in.Reference)
				}
			}
			result = append(result, d)
		}
	}
	return result
}

func (s *State) effectiveResolutions(participant string) map[string]Resolution {
	effective := map[string]Resolution{}
	for _, round := range s.Rounds {
		if round.ParticipantID != participant || !round.Published {
			continue
		}
		for _, q := range round.Questions {
			if r := latestResolution(round, q.ID); r != nil {
				if old, ok := effective[q.ID]; !ok || r.Sequence >= old.Sequence {
					effective[q.ID] = *r
				}
			}
		}
	}
	return effective
}
