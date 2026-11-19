package collaboration

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRequestValidationPreservesImmutableRound(t *testing.T) {
	var s State
	original := []Question{{ID: "Q1", Body: "Use a received-input index?"}, {ID: "Q2", Body: "Keep substantive status comments?"}}
	if _, err := s.Request("R1", "p", "t", "head", original); err != nil {
		t.Fatal(err)
	}
	if err := s.Publish("R1", Source{Channel: "fixture", ID: "1"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		code      string
		questions []Question
		index     int
	}{
		{"question_identity_content_changed", []Question{{ID: "Q1", Body: "Use one updated directory comment?"}, original[1]}, 1},
		{"duplicate_question", []Question{original[0], original[0]}, 2},
		{"unresolved_question_dropped", []Question{original[1]}, 0},
		{"empty_questions_without_wait", nil, 0},
	} {
		t.Run(tc.code, func(t *testing.T) {
			_, err := s.Request("R2", "p", "t2", "head", tc.questions)
			var e *RequestValidationError
			if !errors.As(err, &e) || e.Code != tc.code || e.QuestionIndex != tc.index {
				t.Fatal(err)
			}
			if len(s.Rounds) != 1 || len(s.References) != 1 || s.Rounds[0].Questions[0] != original[0] {
				t.Fatal("history changed")
			}
		})
	}
}

func TestRequestRejectionRecognitionIsStrict(t *testing.T) {
	for _, code := range []string{"question_identity_content_changed", "duplicate_question", "unresolved_question_dropped", "question_limit_exceeded", "empty_questions_without_wait", "earlier_publication_unsettled"} {
		raw, err := RequestRejectionResult(&RequestValidationError{Code: code, QuestionIndex: 1})
		if err != nil || !IsRequestRejection(raw) {
			t.Fatal(string(raw), err)
		}
		for _, bad := range []string{
			strings.Replace(string(raw), `"question_index":1`, `"question_index":null`, 1),
			strings.Replace(string(raw), `"question_index":1`, `"question_index":51`, 1),
			strings.Replace(string(raw), `"question_index":1`, `"question_index":-1`, 1),
			strings.Replace(string(raw), `"question_index":1,`, "", 1),
			strings.Replace(string(raw), code, "dependency-secret", 1),
			string(raw[:len(raw)-1]) + `,"round_id":"R1"}`,
			string(raw[:len(raw)-1]) + `,"outcome":"CLARIFICATION_WAIT"}`,
		} {
			if IsRequestRejection(json.RawMessage(bad)) {
				t.Fatal("recognized malformed rejection", bad)
			}
		}
	}
}
