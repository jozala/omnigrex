package collaboration

import (
	"bytes"
	"encoding/json"
	"io"
)

// RequestValidationError exposes only canonical codes and bounded one-based
// submitted positions, never question bodies or dependency diagnostics.
type RequestValidationError struct {
	Code          string `json:"code"`
	QuestionIndex int    `json:"question_index"`
}

func (e *RequestValidationError) Error() string { return "clarification request rejected: " + e.Code }
func (e *RequestValidationError) Unwrap() error { return ErrInvalid }

type RequestRejection struct {
	OperationID    string                 `json:"operation_id"`
	ParticipantID  string                 `json:"participant_id"`
	TurnID         string                 `json:"turn_id"`
	ExecutionEpoch int64                  `json:"execution_epoch"`
	Validation     RequestValidationError `json:"validation"`
}

// RequestRejectedResult is definite non-publication, not terminal intent.
// Its exact shape is shared by settlement and recovery ledger consumers.
type RequestRejectedResult struct {
	Status        string `json:"status"`
	Publication   string `json:"publication"`
	Reason        string `json:"reason"`
	QuestionIndex int    `json:"question_index"`
	NextAction    string `json:"next_action"`
}

func requestRejectionAction(code string) string {
	switch code {
	case "question_identity_content_changed", "duplicate_question", "unresolved_question_dropped", "question_limit_exceeded", "empty_questions_without_wait":
		return "correct_arguments_new_operation"
	case "earlier_publication_unsettled":
		return "reconcile_prior_operation"
	}
	return ""
}
func (r RequestRejectedResult) valid() bool {
	action := requestRejectionAction(r.Reason)
	positioned := r.Reason == "question_identity_content_changed" || r.Reason == "duplicate_question"
	validIndex := positioned && r.QuestionIndex >= 1 && r.QuestionIndex <= 50 || !positioned && r.QuestionIndex == 0
	return action != "" && validIndex && r.Status == "rejected" && r.Publication == "not_applied" && r.NextAction == action
}
func RequestRejectionResult(e *RequestValidationError) (json.RawMessage, error) {
	r := RequestRejectedResult{Status: "rejected", Publication: "not_applied", Reason: e.Code, QuestionIndex: e.QuestionIndex, NextAction: requestRejectionAction(e.Code)}
	if !r.valid() {
		return nil, ErrInvalid
	}
	return json.Marshal(r)
}
func IsRequestRejection(raw json.RawMessage) bool {
	var r RequestRejectedResult
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&r) != nil || decoder.Decode(new(any)) != io.EOF || !r.valid() {
		return false
	}
	// Zero is meaningful: an omitted or null index is malformed evidence.
	var fields map[string]json.RawMessage
	return json.Unmarshal(raw, &fields) == nil && len(fields) == 5 && fields["question_index"] != nil && string(fields["question_index"]) != "null"
}
