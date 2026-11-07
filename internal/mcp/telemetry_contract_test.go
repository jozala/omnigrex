package mcp

import (
	"github.com/jozala/omnigrex/internal/telemetry"
	"testing"
)

func TestTelemetryToolVocabularyMatchesCatalog(t *testing.T) {
	for _, tool := range toolCatalog {
		if !telemetry.KnownMCPToolName(tool.Name) {
			t.Errorf("missing telemetry tool %s", tool.Name)
		}
	}
	if telemetry.KnownMCPToolName("sentinel-secret") {
		t.Fatal("accepted unknown tool")
	}
}
