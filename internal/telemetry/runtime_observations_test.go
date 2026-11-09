package telemetry

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestLiveToolAndTemporaryFilesystemRecording(t *testing.T) {
	reader := budgetReader(t)
	ctx := context.Background()
	observer := NewAgentTurnObserver("DEVELOPER", nil, nil)
	duration := 1250 * time.Millisecond
	for _, tool := range []agentevent.ToolObservation{
		{Layer: "native", Name: "read", Duration: &duration},
		{Layer: "native", Name: "read", Error: true},
		{Layer: "mcp_client", Name: "get_issue", Duration: &duration},
		{Layer: "native", Name: "SENTINEL_SECRET", Duration: &duration},
	} {
		event := agentevent.AgentEvent{Kind: "activity", TurnID: "SENTINEL_SECRET", Metadata: agentevent.OperationalMetadata{ToolObservation: &tool, ToolCallID: "SENTINEL_SECRET"}}
		if err := observer.Emit(ctx, event); err != nil {
			t.Fatal(err)
		}
		event.Kind = "history_activity"
		if err := observer.Emit(ctx, event); err != nil {
			t.Fatal(err)
		} // replay cannot produce live samples
	}
	value := float64(4096)
	_ = observer.Emit(ctx, agentevent.AgentEvent{Kind: "temporary_filesystems", Metadata: agentevent.OperationalMetadata{TemporaryFilesystems: []agentevent.TemporaryFilesystem{
		{Resource: "tmp", Status: "complete", Capacity: &value, Used: &value, Free: &value, Available: &value, Inodes: &value, FreeInodes: &value},
		{Resource: "run-pi-agent", Status: "unavailable", Used: &value},
	}}})
	var data metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &data); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprintf("%+v", data), "SENTINEL_SECRET") {
		t.Fatal("untrusted attributes escaped")
	}
	names := map[string]bool{}
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			names[m.Name] = true
			switch m.Name {
			case "omnigrex.tool.calls":
				v := m.Data.(metricdata.Sum[int64])
				if len(v.DataPoints) != 3 {
					t.Fatal(v)
				}
				for _, p := range v.DataPoints {
					if p.Value != 1 {
						t.Fatal("replay recorded", p)
					}
				}
			case "omnigrex.tool.duration":
				v := m.Data.(metricdata.Histogram[float64])
				if len(v.DataPoints) != 2 {
					t.Fatal(v)
				}
				for _, p := range v.DataPoints {
					if p.Count != 1 || p.Sum != 1.25 {
						t.Fatal(p)
					}
				}
			case "omnigrex.storage.filesystem.used", "omnigrex.storage.filesystem.capacity":
				v := m.Data.(metricdata.Histogram[float64])
				if len(v.DataPoints) != 1 || v.DataPoints[0].Count != 1 || v.DataPoints[0].Sum != 4096 {
					t.Fatal(v)
				}
			}
		}
	}
	for _, name := range []string{"omnigrex.tool.calls", "omnigrex.tool.duration", "omnigrex.storage.filesystem.used", "omnigrex.storage.filesystem.capacity", "omnigrex.storage.observations"} {
		if !names[name] {
			t.Fatal("missing", name)
		}
	}
	if names["omnigrex.storage.allocated"] {
		t.Fatal("filesystem use is not directory allocation")
	}
}
