//go:build integration

package pi_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/telemetry"
	"go.opentelemetry.io/otel"
	collector "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

// Shared by configuration and retained-Session tests selected on both backends.
// The only exporter is in the controller test process, never inside Pi.
func budgetMetricExport(t *testing.T) func(uint64, uint64, bool, bool) {
	t.Helper()
	received := make(chan *collector.ExportMetricsServiceRequest, 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		request := new(collector.ExportMetricsServiceRequest)
		if err := proto.Unmarshal(body, request); err != nil {
			t.Error(err)
		}
		select {
		case received <- request:
		default:
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	t.Cleanup(server.Close)
	for _, key := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_METRICS_HEADERS", "OTEL_SDK_DISABLED"} {
		t.Setenv(key, "")
	}
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", server.URL+"/v1/metrics")
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
	// Collect once at explicit shutdown, including on slower KVM CI hosts.
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
	previous := otel.GetMeterProvider()
	shutdown, err := telemetry.Init(context.Background(), "budget-qualification")
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := shutdown(ctx); err != nil {
				t.Error(err)
			}
			otel.SetMeterProvider(previous)
		})
	}
	t.Cleanup(stop)
	return func(used, turns uint64, finite, summary bool) {
		t.Helper()
		stop()
		select {
		case request := <-received:
			names := map[string]bool{}
			for _, resource := range request.ResourceMetrics {
				for _, scope := range resource.ScopeMetrics {
					for _, m := range scope.Metrics {
						names[m.Name] = true
						if m.Name == "omnigrex.agent_turn.model_iterations" {
							points := m.GetHistogram().DataPoints
							var sum float64
							var count uint64
							for _, point := range points {
								sum += point.GetSum()
								count += point.Count
							}
							if sum != float64(used) || count != turns {
								t.Fatalf("incorrect usage instrument: %v", m)
							}
						}
					}
				}
			}
			if !names["omnigrex.agent_turn.model_iterations"] || names["omnigrex.agent_turn.step_limit"] != finite || names["omnigrex.agent_turn.budget_utilization"] != finite || names["omnigrex.agent_turn.budget_summary"] != summary || names["omnigrex.agent_turn.budget_handoffs"] {
				t.Fatalf("incorrect budget instruments: %v", names)
			}
		default:
			t.Fatal("no controller metric export")
		}
	}
}
