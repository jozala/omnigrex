package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestPromptEvidenceRejectsHistoricalAndAmbiguousClassifications(t *testing.T) {
	for _, classification := range []string{"end_turn", "max_tokens", "max_turn_requests", "refusal", "cancelled", "unknown"} {
		t.Run(classification, func(t *testing.T) {
			var database Store
			if err := database.CloseMutationAdmissionWithPromptEvidence(context.Background(), AgentTurnLease{}, classification, ""); !errors.Is(err, ErrTerminalCorroborationConflict) {
				t.Fatalf("historical or unknown classification = %v", err)
			}
		})
	}
	if validAgentTurnPromptEvidence("completed", "FAILURE") || validAgentTurnPromptEvidence("", "") {
		t.Fatal("ambiguous or absent prompt evidence was admitted")
	}
}

func TestCorroborationRequiresAdmittedUnlimitedCompletion(t *testing.T) {
	for _, test := range []struct {
		name       string
		outcome    string
		errorClass string
		eligible   bool
	}{
		{name: "complete", outcome: `{"admitted":true,"status":"completed"}`, eligible: true},
		{name: "explicitly unlimited", outcome: `{"status":"completed","limit_reached":false,"admitted":true}`, eligible: true},
		{name: "lost response", errorClass: "FAILURE", eligible: true},
		{name: "deadline after terminal mutation", errorClass: "DEADLINE", eligible: true},
		{name: "unadmitted", outcome: `{"admitted":false,"status":"completed"}`},
		{name: "limited", outcome: `{"admitted":true,"status":"completed","limit_reached":true}`},
		{name: "aborted", outcome: `{"admitted":true,"status":"aborted"}`},
		{name: "unknown", outcome: `{"admitted":true,"status":"unknown"}`},
		{name: "missing admission", outcome: `{"status":"completed"}`},
		{name: "null admission", outcome: `{"admitted":null,"status":"completed"}`},
		{name: "string admission", outcome: `{"admitted":"true","status":"completed"}`},
		{name: "null limit", outcome: `{"admitted":true,"status":"completed","limit_reached":null}`},
		{name: "string limit", outcome: `{"admitted":true,"status":"completed","limit_reached":"false"}`},
		{name: "failure metadata", outcome: `{"admitted":true,"status":"completed","failure_class":"provider"}`},
		{name: "historical completion", outcome: `{"stop_reason":"end_turn"}`},
		{name: "conflicting failure", outcome: `{"admitted":true,"status":"completed"}`, errorClass: "FAILURE"},
		{name: "cancelled", errorClass: "CANCELLATION"},
		{name: "invalid outcome", errorClass: "INVALID_RESPONSE"},
		{name: "malformed", outcome: `{"admitted":true`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := validCorroborationPrompt(json.RawMessage(test.outcome), test.errorClass); got != test.eligible {
				t.Fatalf("corroboration eligibility = %t, want %t", got, test.eligible)
			}
		})
	}
}
