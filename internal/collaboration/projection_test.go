package collaboration_test

import (
	"encoding/json"
	"fmt"
	"github.com/jozala/omnigrex/internal/collaboration"
	"strings"
	"testing"
)

func TestOversizedDiscussionProjectsBoundedEnvelopeWithLosslessRetrieval(t *testing.T) {
	var state collaboration.State
	for i := 0; i < 20; i++ {
		id := fmt.Sprint(i)
		state.Accept("A"+id, collaboration.Command{Kind: "steer", Body: strings.Repeat("\"\\\n<界", 12000)}, admission(id))
	}
	state.Prepare("developer", "T1")
	projection := state.Project("developer", "T1")
	raw, _ := json.Marshal(projection)
	rpc, _ := json.Marshal(map[string]any{"type": "prompt", "message": string(raw)})
	if len(rpc) > 1024*1024 {
		t.Fatalf("oversized RPC: %d", len(rpc))
	}
	var manifest collaboration.Projection
	json.Unmarshal(projection["collaboration_projection"], &manifest)
	if !manifest.RetrievalRequired || manifest.Counts["human_input"] != 20 || manifest.Counts["human_input_history"] != 0 {
		t.Fatal(manifest)
	}
	records := state.Records("developer", "T1", "human_input")
	for index := 0; index < 20; index++ {
		var content strings.Builder
		offset := 0
		for {
			page, err := collaboration.PageRecord(records[index], 20, index, offset)
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(page)
			if len(encoded) > 32*1024 {
				t.Fatalf("unbounded tool result: %d", len(encoded))
			}
			content.WriteString(page.Content)
			if page.NextOffset == nil {
				break
			}
			offset = *page.NextOffset
		}
		if content.String() != string(records[index]) {
			t.Fatal("content/provenance lost across chunk boundary")
		}
	}
	// A later response cannot cross the frozen preparation boundary.
	state.Accept("later", collaboration.Command{Kind: "steer", Body: "Later"}, admission("later"))
	if len(state.Records("developer", "T1", "human_input")) != 20 || !state.Pending("developer") {
		t.Fatal("later input leaked")
	}
}

func TestRetrievalOnlyAssignmentRemainsPendingAndSurvivesUndeliveredTurn(t *testing.T) {
	var state collaboration.State
	for _, id := range []string{"A1", "A2"} {
		state.Accept(id, collaboration.Command{Kind: "steer", Body: strings.Repeat("direction", 8000)}, admission(id))
	}
	state.Prepare("developer", "T1")
	state.Project("developer", "T1")
	if !state.Pending("developer") || state.Unresolved("developer") {
		t.Fatal("omitted steering was treated as delivered")
	}
	if err := state.MarkDelivered("reviewer", "T1", "A1"); err == nil {
		t.Fatal("wrong participant acknowledged delivery")
	}
	if err := state.MarkDelivered("developer", "T0", "A1"); err == nil {
		t.Fatal("wrong Turn acknowledged delivery")
	}
	if err := state.MarkDelivered("developer", "T1", "A1"); err != nil {
		t.Fatal(err)
	}
	if !state.Pending("developer") {
		t.Fatal("one record cleared the entire batch")
	}
	// Retain unread A2 for a retry/continuation; delivered A1 stays history.
	state.Accept("A3", collaboration.Command{Kind: "steer", Body: "Later"}, admission("later"))
	batch := state.Prepare("developer", "T2")
	if len(batch) != 2 || batch[0].ID != "A2" || batch[1].ID != "A3" {
		t.Fatal(batch)
	}
	state.Project("developer", "T2")
	if !state.Pending("developer") {
		t.Fatal("historical size forced retrieval but dropped unread input")
	}
	for _, id := range []string{"A2", "A3"} {
		if err := state.MarkDelivered("developer", "T2", id); err != nil {
			t.Fatal(err)
		}
	}
	if state.Pending("developer") {
		t.Fatal("fully delivered batch remains pending")
	}
}

func TestOmittedResponseCannotBeInterpretedByIdentityAlone(t *testing.T) {
	var s collaboration.State
	s.Request("R1", "developer", "T1", "h", []collaboration.Question{{ID: "Q1", Body: "Policy?"}})
	s.Publish("R1", collaboration.Source{Channel: "fixture", ID: "pub"})
	s.Accept("A1", collaboration.Command{Kind: "answer", Target: "R1", Body: strings.Repeat("explicit context ", 8000)}, admission("a1"))
	s.Prepare("developer", "T2")
	s.Project("developer", "T2")
	resolution := []collaboration.Resolution{{QuestionID: "Q1", ResponseIDs: []string{"A1"}, Explanation: "Interpretation"}}
	if err := s.Resolve("R1", "T2", resolution); err == nil {
		t.Fatal("omitted contents were interpreted by ID alone")
	}
	if err := s.MarkDelivered("developer", "T2", "A1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Resolve("R1", "T2", resolution); err != nil {
		t.Fatal(err)
	}
	if s.Pending("developer") || s.Unresolved("developer") {
		t.Fatal("delivered, interpreted evidence failed to clear guards")
	}
}
