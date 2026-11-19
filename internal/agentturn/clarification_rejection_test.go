package agentturn

import (
	"encoding/json"
	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/store"
	"testing"
)

func TestHandledClarificationRejectionIsNotTerminalIntent(t *testing.T) {
	raw, err := collaboration.RequestRejectionResult(&collaboration.RequestValidationError{Code: "question_identity_content_changed", QuestionIndex: 1})
	if err != nil {
		t.Fatal(err)
	}
	rejected := store.MutationReservation{ID: "rejected", ToolName: mcp.ToolRequestClarification, State: store.MutationSucceeded, Result: raw}
	if intents := successfulTerminalIntents([]store.MutationReservation{rejected}); len(intents) != 0 {
		t.Fatal(intents)
	}
	for _, tool := range []string{mcp.ToolRequestClarification, mcp.ToolReportBlocked, mcp.ToolRequestReview, mcp.ToolSubmitReview, mcp.ToolCompletePlanning} {
		genuine := store.MutationReservation{ID: "genuine", ToolName: tool, State: store.MutationSucceeded, Result: json.RawMessage(`{"outcome":"CLARIFICATION_WAIT"}`)}
		intents := successfulTerminalIntents([]store.MutationReservation{rejected, genuine})
		if len(intents) != 1 || intents[0].ID != "genuine" {
			t.Fatal(tool, intents)
		}
	}
	for _, bad := range []string{`{}`, `{"status":"rejected"}`, `{"outcome":"WRONG"}`, string(raw[:len(raw)-1]) + `,"round_id":"R-new"}`, string(raw[:len(raw)-1]) + `,"outcome":"CLARIFICATION_WAIT"}`} {
		rejected.Result = json.RawMessage(bad)
		if len(successfulTerminalIntents([]store.MutationReservation{rejected})) != 1 {
			t.Fatal("malformed evidence hidden", bad)
		}
	}
}
