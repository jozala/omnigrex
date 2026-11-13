package collaboration

// ResolutionValidationError contains only structural locations and canonical
// codes, never input bodies, caller text, or dependency error strings.
// Indexes are one-based positions in the submitted batch and evidence list.
type ResolutionValidationError struct {
	Code            string `json:"code"`
	ResolutionIndex int    `json:"resolution_index,omitempty"`
	ResponseIndex   int    `json:"response_index,omitempty"`
}

func (e *ResolutionValidationError) Error() string {
	return "clarification resolution validation rejected: " + e.Code
}
func (e *ResolutionValidationError) Unwrap() error { return ErrInvalid }

type ResolutionRejection struct {
	OperationID   string                    `json:"operation_id"`
	ParticipantID string                    `json:"participant_id"`
	Validation    ResolutionValidationError `json:"validation"`
}
