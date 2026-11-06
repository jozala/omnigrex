package agentturn

import (
	"testing"

	"github.com/jozala/omnigrex/internal/store"
)

func TestProvenanceUsesRetainedSourceIdentity(t *testing.T) {
	execution := store.AgentTurnExecutionContext{}
	execution.Turn.AgentProfileCommitSHA = "original-source-commit"
	execution.Turn.AgentProfileContentSHA256 = []byte{0xde, 0xad, 0xbe, 0xef}
	execution.Turn.AgentProfileConfig = []byte(`{"instructions":"sentinel-secret","model":"ignored"}`)
	attrs := executionAttributes(execution)
	found := false
	for _, attr := range attrs {
		if attr.Value.AsString() == "sentinel-secret" {
			t.Fatal("exported snapshot payload")
		}
		if attr.Key == "agent_profile_source_sha256" {
			found = true
			if attr.Value.AsString() != "deadbeef" {
				t.Fatalf("not original source hash: %s", attr.Value.AsString())
			}
		}
	}
	if !found {
		t.Fatal("missing source hash")
	}
}
