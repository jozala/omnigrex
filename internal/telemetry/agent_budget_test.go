package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"github.com/jozala/omnigrex/internal/stepbudget"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func budgetReader(t *testing.T) *metric.ManualReader {
	t.Helper()
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader), metric.WithView(applicationMetricView))
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	metricsActive.Store(true)
	t.Cleanup(func() {
		metricsActive.Store(false)
		otel.SetMeterProvider(previous)
		_ = provider.Shutdown(context.Background())
	})
	return reader
}
func TestAgentBudgetMetrics(t *testing.T) {
	for _, tc := range []struct {
		name, limit, completeness string
		used                      uint64
		complete, summary         bool
	}{
		{"early finite", "10", "complete", 2, true, false},
		{"summary success", "2", "complete", 2, true, true},
		{"unlimited", "unlimited", "complete", 3, true, false},
		{"transport partial", "10", "partial", 2, false, false},
		{"missing terminal zero lower bound", "10", "partial", 0, false, false},
		{"unknown", "", "unknown", 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := budgetReader(t)
			o := NewAgentTurnObserver("DEVELOPER", nil, nil)
			var b *stepbudget.Observation
			if tc.limit != "" {
				limit, _ := stepbudget.Parse(tc.limit)
				b = &stepbudget.Observation{Limit: limit, Used: tc.used, Responses: tc.used, Summary: tc.summary, Complete: tc.complete}
				for range 3 {
					_ = o.Emit(context.Background(), agentevent.AgentEvent{Kind: "step_budget", TurnID: "private-turn", Metadata: agentevent.OperationalMetadata{StepBudget: b, Runtime: []byte(`{"secret":"private-provider-data"}`)}})
				}
			}
			o.Finish(context.Background(), b, "completed")
			o.Finish(context.Background(), b, "completed")
			var data metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &data); err != nil {
				t.Fatal(err)
			}
			names := map[string]bool{}
			for _, scope := range data.ScopeMetrics {
				for _, m := range scope.Metrics {
					names[m.Name] = true
					switch v := m.Data.(type) {
					case metricdata.Sum[int64]:
						if len(v.DataPoints) != 1 || v.DataPoints[0].Value != 1 {
							t.Fatalf("duplicate signal: %+v", v)
						}
						if m.Name == "omnigrex.agent_turn.budget_observations" {
							p := v.DataPoints[0]
							status, _ := p.Attributes.Value("completeness")
							if status.AsString() != tc.completeness || p.Attributes.Len() != 4 {
								t.Fatal(p)
							}
							mode, _ := p.Attributes.Value("budget_mode")
							want := "unknown"
							if b != nil {
								want = "unlimited"
								if b.Limit.Finite() {
									want = "finite"
								}
							}
							if mode.AsString() != want {
								t.Fatal(p)
							}
						}
					case metricdata.Histogram[float64]:
						if len(v.DataPoints) != 1 || v.DataPoints[0].Count != 1 {
							t.Fatalf("duplicate usage: %+v", v)
						}
						p := v.DataPoints[0]
						if p.Attributes.Len() != 3 && m.Name != "omnigrex.agent_turn.step_limit" {
							t.Fatalf("unexpected labels: %v", p.Attributes)
						}
						want := float64(tc.used)
						if m.Name == "omnigrex.agent_turn.step_limit" {
							want = float64(b.Limit.Steps())
						}
						if m.Name == "omnigrex.agent_turn.budget_utilization" {
							want = float64(tc.used) / float64(b.Limit.Steps())
						}
						if p.Sum != want {
							t.Fatalf("%s sum=%v want=%v", m.Name, p.Sum, want)
						}
					}
				}
			}
			if names["omnigrex.agent_turn.budget_handoffs"] {
				t.Fatal("summary entry misclassified as handoff")
			}
			if names["omnigrex.agent_turn.budget_summary"] != tc.summary {
				t.Fatal("missing independent summary signal")
			}
			if names["omnigrex.agent_turn.model_iterations"] != (b != nil) {
				t.Fatal("unknown invented usage")
			}
			finite := b != nil && b.Limit.Finite()
			if names["omnigrex.agent_turn.step_limit"] != finite || names["omnigrex.agent_turn.budget_utilization"] != finite {
				t.Fatal("unlimited invented limit or utilization")
			}
		})
	}
}
func TestBudgetSeriesBudgetAndFiltering(t *testing.T) {
	reader := budgetReader(t)
	for _, role := range []string{"DEVELOPER", "REVIEWER", "private-profile"} {
		for _, termination := range []string{"completed", "failed", "cancelled", "transport", "private-error"} {
			for _, limit := range []string{"1", "unlimited", ""} {
				for _, complete := range []bool{false, true} {
					o := NewAgentTurnObserver(role, nil, nil)
					var b *stepbudget.Observation
					if limit != "" {
						l, _ := stepbudget.Parse(limit)
						b = &stepbudget.Observation{Limit: l, Used: 1, Responses: 1, Complete: complete, Summary: l.Finite()}
					}
					o.Finish(context.Background(), b, termination)
				}
			}
		}
		for _, value := range []string{"1", "unlimited"} {
			configured, _ := stepbudget.Parse(value)
			for _, ending := range []string{"completed", "failed", "cancelled", "transport", "unknown"} {
				NewAgentTurnObserver(role, &configured, nil).Finish(context.Background(), nil, ending)
			}
		}
		RecordBudgetHandoff(context.Background(), role)
	}
	// Even accidentally supplied content/identity dimensions are filtered by views.
	histogram, _ := otel.Meter(Scope).Float64Histogram("omnigrex.agent_turn.model_iterations", otelmetric.WithUnit("{iteration}"))
	histogram.Record(context.Background(), 1, otelmetric.WithAttributes(attribute.String("role", "DEVELOPER"), attribute.String("budget_mode", "finite"), attribute.String("completeness", "complete"), attribute.String("turn_id", "private-id"), attribute.String("summary", "private-content")))
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
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
					if p.Attributes.HasValue("turn_id") || p.Attributes.HasValue("summary") {
						t.Fatal("private dimensions escaped view")
					}
				}
			}
		}
	}
	// 105 coverage + 156 usage + 39 limit + 54 ratio + 6 pressure/handoff.
	if series != 360 {
		t.Fatalf("budget series=%d want 360", series)
	}
}
func TestBudgetReplayInvalidAndDisabled(t *testing.T) {
	reader := budgetReader(t)
	o := NewAgentTurnObserver("private-profile", nil, nil)
	limit, _ := stepbudget.Parse("1")
	b := stepbudget.Observation{Limit: limit, Used: 1, Responses: 1, Summary: true, Complete: true}
	_ = o.Emit(context.Background(), agentevent.AgentEvent{Kind: "history_custom", Metadata: agentevent.OperationalMetadata{StepBudget: &b}})
	invalid := stepbudget.Observation{Used: 1, Responses: 2}
	_ = o.Emit(context.Background(), agentevent.AgentEvent{Kind: "step_budget", Metadata: agentevent.OperationalMetadata{StepBudget: &invalid}})
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.ScopeMetrics) != 0 {
		t.Fatal("replay or invalid evidence created live metrics")
	}
	metricsActive.Store(false)
	o.Finish(context.Background(), &b, "completed")
	RecordBudgetHandoff(context.Background(), "private-profile")
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.ScopeMetrics) != 0 {
		t.Fatal("disabled telemetry recorded metrics")
	}
}

func TestBudgetRecordingDoesNotWaitForFailingExporter(t *testing.T) {
	cleanEnvironment(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", server.URL+"/v1/metrics")
	previous := otel.GetMeterProvider()
	shutdown, err := Init(context.Background(), "failure-test")
	if err != nil {
		t.Fatal(err)
	}
	defer otel.SetMeterProvider(previous)
	limit, _ := stepbudget.Parse("1")
	b := stepbudget.Observation{Limit: limit, Used: 1, Responses: 1, Summary: true, Stopped: true, Complete: true}
	start := time.Now()
	o := NewAgentTurnObserver("DEVELOPER", &limit, nil)
	_ = o.Emit(context.Background(), agentevent.AgentEvent{Kind: "step_budget", Metadata: agentevent.OperationalMetadata{StepBudget: &b}})
	o.Finish(context.Background(), &b, "completed")
	RecordBudgetHandoff(context.Background(), "DEVELOPER")
	if time.Since(start) > time.Second {
		t.Fatal("metric recording waited for network")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := shutdown(ctx); err == nil {
		t.Fatal("failing export unexpectedly succeeded")
	}
	if b.Used != 1 || !b.Complete || !b.Stopped {
		t.Fatal("export changed critical evidence")
	}
}

func TestKnownConfigurationWithUnknownUsage(t *testing.T) {
	for _, value := range []string{"7", "unlimited"} {
		t.Run(value, func(t *testing.T) {
			reader := budgetReader(t)
			limit, _ := stepbudget.Parse(value)
			NewAgentTurnObserver("REVIEWER", &limit, nil).Finish(context.Background(), nil, "transport")
			var data metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &data); err != nil {
				t.Fatal(err)
			}
			observed := false
			for _, scope := range data.ScopeMetrics {
				for _, m := range scope.Metrics {
					switch m.Name {
					case "omnigrex.agent_turn.budget_observations":
						p := m.Data.(metricdata.Sum[int64]).DataPoints[0]
						mode, _ := p.Attributes.Value("budget_mode")
						status, _ := p.Attributes.Value("completeness")
						want := "unlimited"
						if limit.Finite() {
							want = "finite"
						}
						if mode.AsString() != want || status.AsString() != "unknown" {
							t.Fatal(p)
						}
						observed = true
					case "omnigrex.agent_turn.step_limit":
						if !limit.Finite() || m.Data.(metricdata.Histogram[float64]).DataPoints[0].Sum != 7 {
							t.Fatal(m)
						}
					default:
						t.Fatalf("unknown usage invented instrument: %s", m.Name)
					}
				}
			}
			if !observed {
				t.Fatal("missing unknown usage coverage")
			}
		})
	}
}
