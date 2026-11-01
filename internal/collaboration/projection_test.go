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
