package collaboration

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

const DiscoveryProcedureVersion = "omnigrex-discovery/v2"

type OriginalRequest struct {
	Title  string `json:"title"`
	Body   string `json:"body"`
	Source Source `json:"source"`
}
type PlanningContent struct {
	Summary             string   `json:"summary"`
	UnresolvedDecisions []string `json:"unresolved_decisions"`
	Title               string   `json:"title"`
	Status              string   `json:"status"`
	Changes             string   `json:"changes"`
	RequirementsBrief   string   `json:"requirements_brief"`
	TechnicalPlan       string   `json:"technical_plan"`
	InvestigatedCommit  string   `json:"investigated_commit"`
	Evidence            []string `json:"evidence"`
	DecisionResponseIDs []string `json:"decision_response_ids,omitempty"`
}
type PlanningRevision struct {
	Artifact            *RenderedPublication `json:"artifact,omitempty"`
	Reference           string               `json:"reference"`
	SupersedesReference string               `json:"supersedes_reference,omitempty"`
	ID                  string               `json:"id"`
	Supersedes          string               `json:"supersedes,omitempty"`
	ParticipantID       string               `json:"participant_id"`
	TurnID              string               `json:"turn_id"`
	ProcedureVersion    string               `json:"procedure_version"`
	Content             PlanningContent      `json:"content"`
	OriginalRequest     OriginalRequest      `json:"original_request"`
	Decisions           []Input              `json:"decisions,omitempty"`
	PublicationFailed   bool                 `json:"publication_failed,omitempty"`
	Published           bool                 `json:"published"`
	Source              *Source              `json:"source,omitempty"`
}
type PlanRevalidation struct {
	SourceInvocationID  string   `json:"source_invocation_id"`
	RevisionID          string   `json:"revision_id"`
	ParticipantID       string   `json:"participant_id"`
	TurnID              string   `json:"turn_id"`
	RepositoryCommit    string   `json:"repository_commit"`
	Evidence            string   `json:"evidence"`
	ConfirmationInputID string   `json:"confirmation_input_id"`
	ConsideredInputIDs  []string `json:"considered_input_ids"`
}

// CheckpointNotApplied is authoritative pre-publication evidence, not a revision.
type CheckpointNotApplied struct {
	OperationID   string          `json:"operation_id"`
	ParticipantID string          `json:"participant_id"`
	TurnID        string          `json:"turn_id"`
	Content       PlanningContent `json:"content"`
	Reason        string          `json:"reason"`
	Decisions     []StaleDecision `json:"decisions,omitempty"`
}

func (e *CheckpointNotApplied) Error() string { return "checkpoint not applied: " + e.Reason }

// CheckpointInputReason gives retrieval precedence: yielding cannot substitute
// for reading the frozen batch, even when more input has also arrived.
func (s *State) CheckpointInputReason(participant string) string {
	for _, in := range s.Inputs {
		if in.ParticipantID == participant && in.DeliveryPending {
			return "unread_input"
		}
	}
	if s.Pending(participant) {
		return "late_input"
	}
	return ""
}

type PlanningState struct {
	InvalidatedConfirmations []string               `json:"invalidated_confirmations,omitempty"`
	NonPublications          []CheckpointNotApplied `json:"non_publications,omitempty"`
	OriginalRequest          OriginalRequest        `json:"original_request"`
	Revisions                []PlanningRevision     `json:"revisions"`
	CurrentRevision          string                 `json:"current_revision,omitempty"`
	ReadyRevision            string                 `json:"ready_revision,omitempty"`
	Revalidations            []PlanRevalidation     `json:"revalidations,omitempty"`
}

var planningCommit = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func (s *State) StartPlanning(original OriginalRequest) error {
	if s.Planning != nil {
		if s.Planning.OriginalRequest == original {
			return nil
		}
		return ErrInvalid
	}
	if strings.TrimSpace(original.Title) == "" || original.Source.Channel == "" || original.Source.ID == "" || !utf8.ValidString(original.Title+original.Body) {
		return ErrInvalid
	}
	s.Planning = &PlanningState{OriginalRequest: original}
	return nil
}
func cloneRevision(r PlanningRevision) PlanningRevision {
	r.Content.Evidence = slices.Clone(r.Content.Evidence)
	r.Content.UnresolvedDecisions = slices.Clone(r.Content.UnresolvedDecisions)
	r.Content.DecisionResponseIDs = slices.Clone(r.Content.DecisionResponseIDs)
	r.Decisions = slices.Clone(r.Decisions)
	if r.Artifact != nil {
		a := *r.Artifact
		r.Artifact = &a
	}
	if r.Source != nil {
		source := *r.Source
		r.Source = &source
	}
	return r
}
func (s *State) CurrentCheckpoint() *PlanningRevision {
	if s.Planning != nil {
		for _, r := range s.Planning.Revisions {
			if r.ID == s.Planning.CurrentRevision {
				copy := cloneRevision(r)
				return &copy
			}
		}
	}
	return nil
}

// ReserveCheckpoint validates structural presence and provenance, never semantic
// completeness. The full brief and technical plan always share one identity.
func (s *State) ReserveCheckpoint(id, participant, turn string, c PlanningContent) (PlanningRevision, error) {
	if s.Planning == nil || id == "" || participant == "" || turn == "" || s.Planning.ReadyRevision != "" {
		return PlanningRevision{}, ErrInvalid
	}
	for _, rejection := range s.Planning.NonPublications {
		if rejection.OperationID == id {
			if rejection.ParticipantID != participant || rejection.TurnID != turn || !reflect.DeepEqual(rejection.Content, c) {
				return PlanningRevision{}, ErrInvalid
			}
			rejection = cloneCheckpointRejection(rejection)
			return PlanningRevision{}, &rejection
		}
	}
	for _, r := range s.Planning.Revisions {
		if r.ID == id {
			if r.ParticipantID == participant && r.TurnID == turn && reflect.DeepEqual(r.Content, c) {
				return cloneRevision(r), nil
			}
			return PlanningRevision{}, ErrInvalid
		}
		if !r.Published && !r.PublicationFailed {
			return PlanningRevision{}, ErrInvalid
		}
	}
	for _, v := range []string{c.Title, c.Summary, c.Changes, c.RequirementsBrief, c.TechnicalPlan} {
		if strings.TrimSpace(v) == "" || !utf8.ValidString(v) || strings.ContainsRune(v, '\x00') {
			return PlanningRevision{}, ErrInvalid
		}
	}
	if c.Status != "draft" && c.Status != "proposed" || !planningCommit.MatchString(c.InvestigatedCommit) || len(c.Evidence) == 0 {
		return PlanningRevision{}, ErrInvalid
	}
	for _, v := range append(slices.Clone(c.Evidence), c.UnresolvedDecisions...) {
		if strings.TrimSpace(v) == "" || !utf8.ValidString(v) {
			return PlanningRevision{}, ErrInvalid
		}
	}
	if !slices.ContainsFunc(s.Preparations, func(p Preparation) bool { return p.ParticipantID == participant && p.TurnID == turn }) {
		return PlanningRevision{}, ErrInvalid
	}
	supersedesReference := ""
	if current := s.CurrentCheckpoint(); current != nil {
		supersedesReference = current.Reference
	}
	r := PlanningRevision{SupersedesReference: supersedesReference, ID: id, ParticipantID: participant, TurnID: turn, ProcedureVersion: DiscoveryProcedureVersion, Content: c, OriginalRequest: s.Planning.OriginalRequest, Supersedes: s.Planning.CurrentRevision}
	seen := map[string]bool{}
	for _, response := range c.DecisionResponseIDs {
		if seen[response] {
			return PlanningRevision{}, ErrInvalid
		}
		seen[response] = true
		i := slices.IndexFunc(s.Inputs, func(in Input) bool {
			return in.ID == response && in.ParticipantID == participant && in.DeliveredTo != "" && (!in.DeliveryPending || in.DeliveredTo == turn) && in.Kind != "confirm-plan"
		})
		if i < 0 {
			return PlanningRevision{}, ErrInvalid
		}
		r.Decisions = append(r.Decisions, s.Inputs[i])
	}
	if reason := s.CheckpointInputReason(participant); reason != "" {
		s.reference("P", id)
		rejection := CheckpointNotApplied{OperationID: id, ParticipantID: participant, TurnID: turn, Content: c, Reason: reason}
		// Deep-copy slices just as for immutable revisions.
		rejection.Content = cloneRevision(r).Content
		s.Planning.NonPublications = append(s.Planning.NonPublications, rejection)
		rejection.Content = cloneRevision(r).Content
		return PlanningRevision{}, &rejection
	}
	if c.Status == "proposed" {
		if decisions := s.StaleDecisions(participant); len(decisions) > 0 {
			s.reference("P", id)
			rejection := CheckpointNotApplied{OperationID: id, ParticipantID: participant, TurnID: turn, Content: cloneRevision(r).Content, Reason: "historical_decisions_unresolved", Decisions: decisions}
			s.Planning.NonPublications = append(s.Planning.NonPublications, cloneCheckpointRejection(rejection))
			return PlanningRevision{}, &rejection
		}
	}
	if c.Status == "proposed" && (len(c.UnresolvedDecisions) > 0 || s.AssessmentBlocked(participant) || s.Unresolved(participant)) {
		return PlanningRevision{}, ErrInvalid
	}
	r.Reference = s.reference("P", id)
	s.Planning.Revisions = append(s.Planning.Revisions, cloneRevision(r))
	return cloneRevision(r), nil
}
func (s *State) ConfirmCheckpoint(id string, source Source) error {
	if s.Planning == nil || source.Channel == "" || source.ID == "" {
		return ErrInvalid
	}
	for i := range s.Planning.Revisions {
		r := &s.Planning.Revisions[i]
		if r.ID != id {
			continue
		}
		if r.PublicationFailed {
			return ErrInvalid
		}
		if r.Published {
			if r.Source != nil && *r.Source == source {
				return nil
			}
			return ErrInvalid
		}
		r.Published = true
		r.Source = &source
		s.Planning.CurrentRevision = id
		return nil
	}
	return ErrInvalid
}
func (s *State) FailCheckpoint(id string) error {
	if s.Planning != nil {
		for i := range s.Planning.Revisions {
			r := &s.Planning.Revisions[i]
			if r.ID == id && !r.Published {
				r.PublicationFailed = true
				return nil
			}
		}
	}
	return ErrInvalid
}
func (s *State) AwaitingPlanConfirmation(participant string) bool {
	r := s.CurrentCheckpoint()
	return r != nil && r.Published && r.Content.Status == "proposed" && r.ParticipantID == participant && !s.Unresolved(participant) && !s.AssessmentBlocked(participant) && s.Planning.ReadyRevision == ""
}
func (s *State) planningConfirmationTarget(target, participant string) string {
	r := s.CurrentCheckpoint()
	if r == nil || !r.Published || r.Content.Status != "proposed" || r.ID != target || r.ParticipantID != participant || s.Planning.ReadyRevision != "" {
		id := "none"
		if r != nil {
			id = r.Reference
		}
		return fmt.Sprintf("Please review the current proposed pair (%s) and use its short reference, for example `/omnigrex confirm-plan P2`. Draft, replaced and completed pairs cannot be confirmed.", id)
	}
	return ""
}

// ValidatePlanning records final assessment; readiness is committed separately
// at fenced settlement so input arriving after this operation cannot be lost.
func (s *State) ValidatePlanning(operation, revision, participant, turn, commit, evidence string, resolved bool) error {
	if operation == "" || !resolved || !planningCommit.MatchString(commit) || strings.TrimSpace(evidence) == "" || !utf8.ValidString(evidence) || s.Pending(participant) || s.Unresolved(participant) || s.AssessmentBlocked(participant) {
		return ErrInvalid
	}
	r := s.CurrentCheckpoint()
	if r == nil || !r.Published || r.ID != revision || r.ParticipantID != participant || r.Content.Status != "proposed" {
		return ErrInvalid
	}
	if !slices.ContainsFunc(s.Preparations, func(p Preparation) bool { return p.ParticipantID == participant && p.TurnID == turn }) {
		return ErrInvalid
	}
	v := PlanRevalidation{SourceInvocationID: operation, RevisionID: revision, ParticipantID: participant, TurnID: turn, RepositoryCommit: commit, Evidence: evidence}
	for _, in := range s.Inputs {
		if in.ParticipantID != participant || in.DeliveredTo == "" || in.DeliveryPending {
			continue
		}
		v.ConsideredInputIDs = append(v.ConsideredInputIDs, in.ID)
		if in.Kind == "confirm-plan" && in.Target == revision && !slices.Contains(s.Planning.InvalidatedConfirmations, in.ID) && s.latestAssessment(in.ID) != nil && s.latestAssessment(in.ID).Disposition == "confirmed" {
			v.ConfirmationInputID = in.ID
		}
	}
	if v.ConfirmationInputID == "" {
		return ErrInvalid
	}
	for _, old := range s.Planning.Revalidations {
		if old.TurnID == turn {
			if reflect.DeepEqual(old, v) {
				return nil
			}
			return ErrInvalid
		}
	}
	if s.Planning.ReadyRevision != "" {
		return ErrInvalid
	}
	s.Planning.Revalidations = append(s.Planning.Revalidations, v)
	return nil
}

// PlanningValidationCurrent rechecks authority at settlement, not merely when
// the completion tool ran. An immutable old validation is not a current grant.
func (s *State) PlanningValidationCurrent(v PlanRevalidation) bool {
	if s.Planning == nil || s.Planning.CurrentRevision != v.RevisionID || s.Pending(v.ParticipantID) || s.AssessmentBlocked(v.ParticipantID) || s.Unresolved(v.ParticipantID) || slices.Contains(s.Planning.InvalidatedConfirmations, v.ConfirmationInputID) {
		return false
	}
	a := s.latestAssessment(v.ConfirmationInputID)
	if a == nil || a.Disposition != "confirmed" {
		return false
	}
	for _, in := range s.Inputs {
		if in.ParticipantID == v.ParticipantID && !slices.Contains(v.ConsideredInputIDs, in.ID) {
			return false
		}
	}
	return true
}

func cloneCheckpointRejection(r CheckpointNotApplied) CheckpointNotApplied {
	r.Content = cloneRevision(PlanningRevision{Content: r.Content}).Content
	r.Decisions = slices.Clone(r.Decisions)
	for i := range r.Decisions {
		r.Decisions[i].EvidenceReferences = slices.Clone(r.Decisions[i].EvidenceReferences)
		r.Decisions[i].CorrectionReferences = slices.Clone(r.Decisions[i].CorrectionReferences)
	}
	return r
}
