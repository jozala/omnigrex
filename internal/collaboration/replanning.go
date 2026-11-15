package collaboration

import (
	"encoding/json"
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

// Equal preserves exact concern content across JSON persistence. An omitted
// optional list and an explicitly empty list both mean no human-origin evidence.
func (c ReplanningConcern) Equal(other ReplanningConcern) bool {
	return c.AcceptedRevisionID == other.AcceptedRevisionID && c.Commitment == other.Commitment && slices.Equal(c.Evidence, other.Evidence) && c.Consequences == other.Consequences && c.Alternative == other.Alternative && slices.Equal(c.HumanInputIDs, other.HumanInputIDs)
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
	EvidenceInputIDs       []string               `json:"evidence_input_ids,omitempty"`
	EvidenceDelivered      bool                   `json:"evidence_delivered,omitempty"`
	EvidenceTurnID         string                 `json:"evidence_turn_id,omitempty"`
	Resolutions            []ReplanningResolution `json:"resolutions,omitempty"`
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
		v.ConsideredInputIDs = slices.Clone(v.ConsideredInputIDs)
		c.Resolution = &v
	}
	c.EvidenceInputIDs = slices.Clone(c.EvidenceInputIDs)
	c.Resolutions = slices.Clone(c.Resolutions)
	for i := range c.Resolutions {
		c.Resolutions[i].ConsideredInputIDs = slices.Clone(c.Resolutions[i].ConsideredInputIDs)
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
				if c.Scope == scope && c.Concern.Equal(concern) {
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
				if slices.Contains(old.Concern.HumanInputIDs, id) || slices.Contains(old.EvidenceInputIDs, id) || old.Resolution != nil && slices.Contains(old.Resolution.ConsideredInputIDs, id) {
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
	return c != nil && c.Phase != ReplanningRequested && c.Scope.PlannerParticipantID == participant && slices.Contains(c.EvidenceInputIDs, in.ID)
}

type ReplanningResolution struct {
	AuthorizationID    string   `json:"authorization_id"`
	WorkHeadSHA        string   `json:"work_head_sha,omitempty"`
	ConsideredInputIDs []string `json:"considered_input_ids"`
	OperationID        string   `json:"operation_id"`
	TurnID             string   `json:"turn_id"`
	RevisionID         string   `json:"revision_id"`
	RepositoryCommit   string   `json:"repository_commit"`
	Evidence           string   `json:"evidence"`
	Disposition        string   `json:"disposition"`
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
				for _, in := range s.Inputs {
					if in.ParticipantID == c.Scope.DeveloperParticipantID {
						c.EvidenceInputIDs = append(c.EvidenceInputIDs, in.ID)
					}
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
	if c == nil || c.Phase != ReplanningActive && c.Phase != ReplanningQueued || c.Scope.PlannerParticipantID != participant || !c.EvidenceDelivered || v.OperationID == "" || v.TurnID == "" || !planningCommit.MatchString(v.RepositoryCommit) || strings.TrimSpace(v.Evidence) == "" || !slices.ContainsFunc(s.Preparations, func(p Preparation) bool { return p.ParticipantID == participant && p.TurnID == v.TurnID }) || s.Pending(participant) || s.Unresolved(participant) || s.AssessmentBlocked(participant) {
		return ErrInvalid
	}
	reuseExisting := v.Disposition == "unchanged" && s.existingImplementationAuthorityValid()
	if g := s.ImplementationGrant(); g != nil && g.ID != s.Planning.Implementation.AuthorizationID {
		reuseExisting = false
	}
	if v.Disposition == "unchanged" {
		if v.RevisionID != c.Concern.AcceptedRevisionID || s.Planning.CurrentRevision != v.RevisionID {
			return ErrInvalid
		}
		if !reuseExisting && !s.ImplementationValidated() {
			return ErrInvalid
		}
	} else if v.Disposition == "revised" {
		if s.Planning.CurrentRevision != v.RevisionID || !s.ImplementationValidated() {
			return ErrInvalid
		}
	} else {
		return ErrInvalid
	}
	v.ConsideredInputIDs = nil
	for _, in := range s.Inputs {
		if s.InputInScope(in, participant) {
			v.ConsideredInputIDs = append(v.ConsideredInputIDs, in.ID)
		}
	}
	if reuseExisting {
		v.AuthorizationID = s.Planning.Implementation.AuthorizationID
	} else {
		v.AuthorizationID = s.ImplementationGrant().ID
	}
	for i := range s.Replanning.Cycles {
		if s.Replanning.Cycles[i].Scope.OperationID == c.Scope.OperationID {
			s.Replanning.Cycles[i].Resolutions = append(s.Replanning.Cycles[i].Resolutions, v)
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
	if !slices.ContainsFunc(b.Actions, func(a ImplementationAction) bool {
		return a.ID == b.AuthorizationID && a.Kind == "implement" && a.RevisionID == b.AcceptedRevisionID && a.Actor.ID != "" && a.Source.Channel != "" && a.Source.ID != ""
	}) {
		return false
	}
	// A later grant cannot resurrect the consumed binding after withdrawal.
	// It must instead be explicitly validated and consumed as fresh authority.
	found := false
	for _, a := range b.Actions {
		if a.ID == b.AuthorizationID {
			found = true
			continue
		}
		if found && a.Kind == "withdraw-implementation" && a.RevisionID == b.AcceptedRevisionID {
			return false
		}
	}

	return true
}

func (s *State) InputDeliveredInScope(in Input, participant string) bool {
	if in.ParticipantID == participant {
		return in.DeliveredTo != "" && !in.DeliveryPending
	}
	c := s.ActiveReplanning()
	return s.InputInScope(in, participant) && c != nil && c.EvidenceDelivered
}
func (s *State) RoundInScope(r Round, participant string) bool {
	if r.ParticipantID == participant {
		return true
	}
	c := s.ActiveReplanning()
	return c != nil && c.Phase != ReplanningRequested && c.Scope.PlannerParticipantID == participant && c.Scope.DeveloperParticipantID == r.ParticipantID
}
func (s *State) ReplanningEvidenceDelivered(participant, turn string) error {
	if s.Replanning != nil {
		for i := range s.Replanning.Cycles {
			c := &s.Replanning.Cycles[i]
			if c.Scope.PlannerParticipantID == participant && c.EvidenceTurnID == turn && c.Phase != ReplanningCompleted {
				c.EvidenceDelivered = true
				return nil
			}
		}
	}
	return ErrInvalid
}
func (s *State) ReplanningRecord(participant string) json.RawMessage {
	c := s.ActiveReplanning()
	if c == nil && s.Replanning != nil && len(s.Replanning.Cycles) > 0 && slices.ContainsFunc(s.Preparations, func(p Preparation) bool { return p.ParticipantID == participant }) {
		latest := cloneReplanningCycle(s.Replanning.Cycles[len(s.Replanning.Cycles)-1])
		c = &latest
	}
	if c == nil {
		return nil
	}
	if c.Phase != ReplanningCompleted && participant != c.Scope.PlannerParticipantID && participant != c.Scope.DeveloperParticipantID {
		return nil
	}
	c.EvidenceDelivered = false
	c.EvidenceTurnID = ""
	var inputs []Input
	for _, in := range s.Inputs {
		if slices.Contains(c.EvidenceInputIDs, in.ID) || c.Resolution != nil && slices.Contains(c.Resolution.ConsideredInputIDs, in.ID) {
			inputs = append(inputs, in)
		}
	}
	raw, _ := json.Marshal(struct {
		Cycle  ReplanningCycle `json:"cycle"`
		Inputs []Input         `json:"inputs"`
	}{*c, inputs})
	return raw
}

func (s *State) ResumptionReady() bool {
	c := s.ActiveReplanning()
	if c == nil || c.Phase != ReplanningQueued || c.Resolution == nil || s.Planning == nil || s.Planning.CurrentRevision != c.Resolution.RevisionID {
		return false
	}
	p := c.Scope.PlannerParticipantID
	if s.Pending(p) || s.Unresolved(p) || s.AssessmentBlocked(p) {
		return false
	}
	for _, in := range s.Inputs {
		if s.InputInScope(in, p) && !slices.Contains(c.Resolution.ConsideredInputIDs, in.ID) {
			return false
		}
	}
	if c.Resolution.AuthorizationID == s.Planning.Implementation.AuthorizationID {
		return c.Resolution.Disposition == "unchanged" && s.existingImplementationAuthorityValid()
	}
	g := s.ImplementationGrant()
	return g != nil && g.ID == c.Resolution.AuthorizationID && s.ImplementationValidated()
}
func (s *State) AdmitResumption(turn string) error {
	if turn == "" || !s.ResumptionReady() {
		return ErrInvalid
	}
	c := s.ActiveReplanning()
	b := &s.Planning.Implementation
	if len(b.Bindings) == 0 {
		// The original binding is retained before installing a revised pointer.
		if b.FirstDeveloperTurnID == "" || b.AcceptedRevisionID == "" || b.AuthorizationID == "" {
			return ErrInvalid
		}
		b.Bindings = append(b.Bindings, ImplementationBinding{TurnID: b.FirstDeveloperTurnID, RevisionID: b.AcceptedRevisionID, AuthorizationID: b.AuthorizationID})
	}
	authorization := c.Resolution.AuthorizationID
	b.Bindings = append(b.Bindings, ImplementationBinding{CycleID: c.Scope.OperationID, TurnID: turn, RevisionID: c.Resolution.RevisionID, AuthorizationID: authorization})
	b.AcceptedRevisionID = c.Resolution.RevisionID
	b.AuthorizationID = authorization
	for i := range s.Replanning.Cycles {
		if s.Replanning.Cycles[i].Scope.OperationID == c.Scope.OperationID {
			s.Replanning.Cycles[i].Phase = ReplanningCompleted
			s.Replanning.Cycles[i].ResumedDeveloperTurnID = turn
		}
	}
	return nil
}

// Reconsider leaves all resolution receipts and charges in history. New evidence
// or repository movement does not allocate another cycle or restore consent.
func (s *State) ReconsiderReplanning() error {
	if s.Replanning != nil {
		for i := range s.Replanning.Cycles {
			c := &s.Replanning.Cycles[i]
			if c.Phase == ReplanningQueued || c.Phase == ReplanningActive {
				c.Phase = ReplanningActive
				s.Planning.ReadyRevision = ""
				return nil
			}
		}
	}
	return ErrInvalid
}
