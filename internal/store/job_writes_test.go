package store

import (
	"encoding/json"
	"testing"
)

func TestValidateJobInsertPreservesJSONIntegers(t *testing.T) {
	payload := json.RawMessage(`{"revision":9007199254740993}`)
	validated, err := validateJobInsert(jobInsert{
		queue:          "workflow",
		kind:           "TEST",
		payload:        payload,
		maxAttempts:    1,
		idempotencyKey: "large-json-integer",
		scope:          jobInsertScope{workflowID: "10000000-0000-4000-8000-000000000001"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(validated.payload) != string(payload) {
		t.Fatalf("validated payload = %s, want %s", validated.payload, payload)
	}
}
