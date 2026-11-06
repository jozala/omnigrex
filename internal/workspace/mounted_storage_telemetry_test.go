package workspace

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/telemetry"
	"go.opentelemetry.io/otel"
	collector "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

func TestMountedDiscoveryExportsHealthyUsageAndFailedCategories(t *testing.T) {
	// Isolated local exporter: never inherit deployment endpoints or credentials.
	for _, name := range []string{"OTEL_SDK_DISABLED", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_METRICS_HEADERS", "OTEL_EXPORTER_OTLP_COMPRESSION", "OTEL_EXPORTER_OTLP_METRICS_COMPRESSION", "OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES"} {
		t.Setenv(name, "")
	}
	previous, handler := otel.GetMeterProvider(), otel.GetErrorHandler()
	t.Cleanup(func() { otel.SetMeterProvider(previous); otel.SetErrorHandler(handler) })
	received := make(chan *collector.ExportMetricsServiceRequest, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		request := new(collector.ExportMetricsServiceRequest)
		if err := proto.Unmarshal(body, request); err != nil {
			t.Error(err)
			return
		}
		select {
		case received <- request:
		default:
			t.Error("unexpected repeated export")
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer backend.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", backend.URL+"/v1/metrics")
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
	t.Setenv("OTEL_METRIC_EXPORT_TIMEOUT", "5000")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TIMEOUT", "5000")
	shutdown, err := telemetry.Init(context.Background(), "storage-test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			t.Error(err)
		}
	}()
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "assignment-10000000-0000-4000-8000-000000000001", "workspace"), 0700); err != nil {
		t.Fatal(err)
	}
	failed := filepath.Join(t.TempDir(), "sentinel-private-root")
	if err := os.WriteFile(failed, nil, 0600); err != nil {
		t.Fatal(err)
	}
	monitor, err := NewToolCacheMonitor(&Lifecycle{workspaceRoot: workspace, miseRoot: failed}, ToolCacheMonitorConfig{StateRoot: failed, ThresholdBytes: 1, PollInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer monitor.Close()
	if checked, _, err := monitor.Check(context.Background()); err != nil || checked != 1 {
		t.Fatalf("check=%d %v", checked, err)
	}
	// ForceFlush uses the existing provider, and export happens only in this test.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := otel.GetMeterProvider().(interface{ ForceFlush(context.Context) error }).ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-received:
		if strings.Contains(request.String(), "sentinel-private-root") {
			t.Fatal("exported backing path")
		}
		unavailable, healthyUsage := map[string]bool{}, false
		for _, resource := range request.ResourceMetrics {
			for _, scope := range resource.ScopeMetrics {
				for _, instrument := range scope.Metrics {
					for _, point := range instrument.GetSum().GetDataPoints() {
						category, status := "", ""
						for _, attr := range point.Attributes {
							if attr.Key == "resource" {
								category = attr.Value.GetStringValue()
							}
							if attr.Key == "status" {
								status = attr.Value.GetStringValue()
							}
						}
						if instrument.Name == "omnigrex.storage.observations" && status == "unavailable" && point.GetAsInt() > 0 {
							unavailable[category] = true
						}
					}
					if instrument.Name == "omnigrex.storage.allocated" {
						for _, point := range instrument.GetHistogram().GetDataPoints() {
							for _, attr := range point.Attributes {
								if attr.Key == "resource" && attr.Value.GetStringValue() == "workspace" && point.Count > 0 {
									healthyUsage = true
								}
							}
						}
					}
				}
			}
		}
		if !healthyUsage || !unavailable["state"] || !unavailable["mise"] || !unavailable["tool-data"] {
			t.Fatalf("healthy usage=%v unavailable categories=%v", healthyUsage, unavailable)
		}
	case <-ctx.Done():
		t.Fatal("missing local telemetry export")
	}
}
