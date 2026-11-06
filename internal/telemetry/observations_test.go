package telemetry

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestObservationSeriesBudget(t *testing.T) {
	reader := budgetReader(t)
	ctx := context.Background()
	duration := time.Second
	for _, layer := range []string{"mcp_server", "mcp_client", "native"} {
		names := mcpToolNames
		if layer == "native" {
			names = nativeToolNames
		}
		for _, name := range names {
			for _, outcome := range []Outcome{Success, DomainOutcome, Failure, Cancelled, Timeout} {
				RecordTool(ctx, layer, name, outcome, &duration)
			}
		}
	}
	for _, confirmed := range []bool{false, true} {
		RecordPromptDuration(ctx, duration, confirmed)
	}
	bytes := int64(4096)
	for _, resource := range storageResources {
		for _, status := range storageStatuses {
			RecordStorage(ctx, resource, status, &bytes)
		}
	}
	capacity := float64(1 << 30)
	RecordFilesystem(ctx, &capacity, &capacity, &capacity, &capacity, &capacity)
	var data metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &data); err != nil {
		t.Fatal(err)
	}
	series := 0
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			switch v := m.Data.(type) {
			case metricdata.Sum[int64]:
				series += len(v.DataPoints)
			case metricdata.Histogram[float64]:
				for _, p := range v.DataPoints {
					series += len(p.BucketCounts) + 2
				}
			default:
				t.Fatalf("unexpected instrument %s: %T", m.Name, m.Data)
			}
		}
	}
	if series != 1299 || series+653 > 2050 {
		t.Fatalf("extension=%d total=%d target=2050", series, series+653)
	}
}

func TestUnknownNamesAndMissingDurations(t *testing.T) {
	reader := budgetReader(t)
	ctx := context.Background()
	duration := time.Second
	RecordTool(ctx, "native", "sentinel-secret", Success, &duration)
	RecordTool(ctx, "sentinel-secret", "read", Success, &duration)
	RecordTool(ctx, "native", "read", Outcome("sentinel-secret"), &duration)
	RecordStorage(ctx, "sentinel-secret", "complete", nil)
	RecordStorage(ctx, "tmp", "sentinel-secret", nil)
	// An interrupted call is counted as cancelled without fabricating a duration.
	RecordTool(ctx, "native", "read", Cancelled, nil)
	RecordStorage(ctx, "tmp", "missing", nil)
	var data metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &data); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprintf("%+v", data), "sentinel-secret") {
		t.Fatal("untrusted label escaped")
	}
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == "omnigrex.tool.duration" || m.Name == "omnigrex.storage.allocated" {
				t.Fatalf("fabricated sample: %s", m.Name)
			}
			if v, ok := m.Data.(metricdata.Sum[int64]); !ok || len(v.DataPoints) != 1 || v.DataPoints[0].Value != 1 {
				t.Fatalf("calls: %+v", m)
			}
		}
	}
}

func TestStorageEmptyAndUnavailableAreDifferent(t *testing.T) {
	reader := budgetReader(t)
	ctx := context.Background()
	zero := int64(0)
	RecordStorage(ctx, "tmp", "complete", &zero)
	RecordStorage(ctx, "tmp", "missing", &zero)
	RecordStorage(ctx, "tmp", "unavailable", &zero)
	RecordStorage(ctx, "tmp", "partial", nil)
	var data metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &data); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == "omnigrex.storage.allocated" {
				found = true
				h := m.Data.(metricdata.Histogram[float64])
				if len(h.DataPoints) != 1 || h.DataPoints[0].Count != 1 || h.DataPoints[0].Sum != 0 {
					t.Fatalf("false zero samples: %+v", h)
				}
				status, _ := h.DataPoints[0].Attributes.Value("status")
				if status.AsString() != "complete" {
					t.Fatalf("false status %s", status.AsString())
				}
			}
		}
	}
	if !found {
		t.Fatal("observed empty sample omitted")
	}
}
