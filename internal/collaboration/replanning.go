package collaboration

import (
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"
)

// ReplanningConcern is evidence, not a grant to change accepted commitments.
// Scope, origin and dispatch accounting are derived by the Orchestrator.
type ReplanningConcern struct {
	AcceptedRevisionID string   `json:"accepted_revision_id"`
	Commitment         string   `json:"commitment"`
	Evidence           []string `json:"evidence"`
	Consequences       string   `json:"consequences"`
	Alternative        string   `json:"alternative,omitempty"`
	HumanInputIDs      []string `json:"human_input_ids,omitempty"`
}

type ReplanningOrigin string

const (
	ReplanningAutonomous ReplanningOrigin = "autonomous"
	ReplanningHuman      ReplanningOrigin = "human"
)

type ReplanningPhase string

const (
	ReplanningRequested ReplanningPhase = "requested"
	ReplanningActive    ReplanningPhase = "active"
	ReplanningQueued    ReplanningPhase = "resumption_queued"
	ReplanningCompleted ReplanningPhase = "completed"
)

// ReplanningRequestScope contains trusted durable scope, never agent arguments.
type ReplanningRequestScope struct {
	OperationID            string `json:"operation_id"`
	TurnID                 string `json:"turn_id"`
	ExecutionEpoch         uint64 `json:"execution_epoch"`
	AttemptID              string `json:"attempt_id"`
	DeveloperParticipantID string `json:"developer_participant_id"`
	PlannerParticipantID   string `json:"planner_participant_id"`
	ChangeProposalID       int64  `json:"change_proposal_id,omitempty"`
	HeadSHA                string `json:"head_sha"`
}
type ReplanningCycle struct {
	DispatchedAttemptID    string                 `json:"dispatched_attempt_id,omitempty"`
	Resolution             *ReplanningResolution  `json:"resolution,omitempty"`
	ResumedDeveloperTurnID string                 `json:"resumed_developer_turn_id,omitempty"`
	Scope                  ReplanningRequestScope `json:"scope"`
	Concern                ReplanningConcern      `json:"concern"`
	AuthorizationID        string                 `json:"authorization_id"`
	Origin                 ReplanningOrigin       `json:"origin"`
	Phase                  ReplanningPhase        `json:"phase"`
}
type ReplanningState struct {
	Cycles []ReplanningCycle `json:"cycles"`
}

func cloneReplanningCycle(c ReplanningCycle) ReplanningCycle {
	if c.Resolution != nil {
		v := *c.Resolution
		c.Resolution = &v
	}
	c.Concern.Evidence = slices.Clone(c.Concern.Evidence)
	c.Concern.HumanInputIDs = slices.Clone(c.Concern.HumanInputIDs)
	return c
}
func (s *State) ActiveReplanning() *ReplanningCycle {
	if s.Replanning != nil {
		for i := len(s.Replanning.Cycles) - 1; i >= 0; i-- {
			if s.Replanning.Cycles[i].Phase != ReplanningCompleted {
				c := cloneReplanningCycle(s.Replanning.Cycles[i])
				return &c
			}
		}
	}
	return nil
}

// RequestReplanning records a request bound to one admitted operation. It neither
// dispatches a Planner nor charges a cycle; fenced settlement owns that boundary.
func (s *State) RequestReplanning(scope ReplanningRequestScope, concern ReplanningConcern) (ReplanningCycle, error) {
	if s.Replanning != nil {
		for _, c := range s.Replanning.Cycles {
			if c.Scope.OperationID == scope.OperationID {
				if c.Scope == scope && reflect.DeepEqual(c.Concern, concern) {
					return cloneReplanningCycle(c), nil
				}
				return ReplanningCycle{}, ErrInvalid
			}
		}
	}
	invalid := func() (ReplanningCycle, error) { return ReplanningCycle{}, ErrInvalid }
	if s.Planning == nil || s.ActiveReplanning() != nil || scope.OperationID == "" || scope.TurnID == "" || scope.ExecutionEpoch == 0 || scope.AttemptID == "" || scope.DeveloperParticipantID == "" || scope.PlannerParticipantID == "" || scope.DeveloperParticipantID == scope.PlannerParticipantID || !planningCommit.MatchString(scope.HeadSHA) || scope.ChangeProposalID < 0 {
		return invalid()
	}
	binding := s.Planning.Implementation
	if binding.FirstDeveloperTurnID == "" || binding.AcceptedRevisionID == "" || binding.AuthorizationID == "" || concern.AcceptedRevisionID != binding.AcceptedRevisionID {
		return invalid()
	}
	if !slices.ContainsFunc(s.Planning.Revisions, func(r PlanningRevision) bool {
		return r.ID == binding.AcceptedRevisionID && r.Published && r.Source != nil && r.Content.Status == "proposed" && r.ParticipantID == scope.PlannerParticipantID
	}) || !slices.ContainsFunc(s.Preparations, func(p Preparation) bool {
		return p.ParticipantID == scope.DeveloperParticipantID && p.TurnID == scope.TurnID
	}) {
		return invalid()
	}
	text := func(v string, max int, required bool) bool {
		return (!required || strings.TrimSpace(v) != "") && utf8.ValidString(v) && !strings.ContainsRune(v, '\x00') && utf8.RuneCountInString(v) <= max
	}
	if !text(concern.Commitment, 8192, true) || !text(concern.Consequences, 8192, true) || !text(concern.Alternative, 8192, false) || len(concern.Evidence) == 0 || len(concern.Evidence) > 50 || len(concern.HumanInputIDs) > 50 {
		return invalid()
	}
	for _, e := range concern.Evidence {
		if !text(e, 8192, true) {
			return invalid()
		}
	}
	origin := ReplanningAutonomous
	seen := map[string]bool{}
	for _, id := range concern.HumanInputIDs {
		if seen[id] {
			return invalid()
		}
		seen[id] = true
		index := slices.IndexFunc(s.Inputs, func(in Input) bool {
			return in.ID == id && in.ParticipantID == scope.DeveloperParticipantID && in.DeliveredTo != "" && !in.DeliveryPending
		})
		if index < 0 {
			return invalid()
		}
		a := s.latestAssessment(id)
		if a == nil || a.Disposition == "uncertain" {
			return invalid()
		}
		in := s.Inputs[index]
		independent := true
		if s.Replanning != nil {
			for _, old := range s.Replanning.Cycles {
				if slices.Contains(old.Concern.HumanInputIDs, id) {
					independent = false
				}
			}
		}
		if independent && (in.Kind == "steer" || in.Kind == "amend") && a.Disposition == "changed" && a.InvalidateConfirmation {
			origin = ReplanningHuman
		}
	}
	c := ReplanningCycle{Scope: scope, Concern: concern, AuthorizationID: binding.AuthorizationID, Origin: origin, Phase: ReplanningRequested}
	if s.Replanning == nil {
		s.Replanning = &ReplanningState{}
	}
	s.Replanning.Cycles = append(s.Replanning.Cycles, cloneReplanningCycle(c))
	return cloneReplanningCycle(c), nil
}

// InputInScope shares explicit cycle evidence without reparenting its immutable
// owner, source or target. No unrelated cross-Participant input is admitted.
func (s *State) InputInScope(in Input, participant string) bool {
	if in.ParticipantID == participant {
		return true
	}
	c := s.ActiveReplanning()
	return c != nil && c.Phase != ReplanningRequested && c.Scope.PlannerParticipantID == participant && slices.Contains(c.Concern.HumanInputIDs, in.ID)
}

type ReplanningResolution struct {
	OperationID      string `json:"operation_id"`
	TurnID           string `json:"turn_id"`
	RevisionID       string `json:"revision_id"`
	RepositoryCommit string `json:"repository_commit"`
	Evidence         string `json:"evidence"`
	Disposition      string `json:"disposition"`
}

func (s *State) ActivateReplanning(operation, attempt string) error {
	if s.Replanning != nil {
		for i := range s.Replanning.Cycles {
			c := &s.Replanning.Cycles[i]
			if c.Scope.OperationID == operation {
				if c.Phase == ReplanningActive && c.DispatchedAttemptID == attempt {
					return nil
				}
				if c.Phase != ReplanningRequested || attempt == "" {
					return ErrInvalid
				}
				c.Phase = ReplanningActive
				c.DispatchedAttemptID = attempt
				s.Planning.ReadyRevision = ""
				return nil
			}
		}
	}
	return ErrInvalid
}
func (s *State) ResolveReplanning(participant string, v ReplanningResolution) error {
	c := s.ActiveReplanning()
	if c == nil || c.Phase != ReplanningActive || c.Scope.PlannerParticipantID != participant || v.OperationID == "" || v.TurnID == "" || !planningCommit.MatchString(v.RepositoryCommit) || strings.TrimSpace(v.Evidence) == "" || !slices.ContainsFunc(s.Preparations, func(p Preparation) bool { return p.ParticipantID == participant && p.TurnID == v.TurnID }) || s.Pending(participant) || s.Unresolved(participant) || s.AssessmentBlocked(participant) {
		return ErrInvalid
	}
	if v.Disposition == "unchanged" {
		if v.RevisionID != c.Concern.AcceptedRevisionID || s.Planning.CurrentRevision != v.RevisionID || !s.existingImplementationAuthorityValid() {
			return ErrInvalid
		}
	} else if v.Disposition == "revised" {
		if v.RevisionID == c.Concern.AcceptedRevisionID || s.Planning.CurrentRevision != v.RevisionID || !s.ImplementationValidated() {
			return ErrInvalid
		}
	} else {
		return ErrInvalid
	}
	for i := range s.Replanning.Cycles {
		if s.Replanning.Cycles[i].Scope.OperationID == c.Scope.OperationID {
			s.Replanning.Cycles[i].Resolution = &v
			s.Replanning.Cycles[i].Phase = ReplanningQueued
			return nil
		}
	}
	return ErrInvalid
}
func (s *State) existingImplementationAuthorityValid() bool {
	if s.Planning == nil {
		return false
	}
	b := s.Planning.Implementation
	if b.AuthorizationID == "" || slices.Contains(b.Invalidated, b.AuthorizationID) || slices.Contains(s.Planning.InvalidatedConfirmations, b.AuthorizationID) {
		return false
	}
	if len(b.Actions) > 0 {
		last := b.Actions[len(b.Actions)-1]
		if last.Kind == "withdraw-implementation" && last.RevisionID == b.AcceptedRevisionID {
			return false
		}
	}
	return true
}
