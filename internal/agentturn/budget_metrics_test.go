package agentturn_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/stepbudget"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/telemetry"
	"github.com/jozala/omnigrex/internal/workflow"
	"go.opentelemetry.io/otel"
	collector "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

func TestExecutionBudgetMetricsFollowSettlementNotSummaryPressure(t *testing.T) {
	for _, exhausted := range []bool{false, true} {
		t.Run(map[bool]string{false: "successful summary boundary", true: "settled exhausted work"}[exhausted], func(t *testing.T) {
			received := make(chan *collector.ExportMetricsServiceRequest, 2)
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
			defer server.Close()
			for _, key := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_METRICS_HEADERS", "OTEL_SDK_DISABLED"} {
				t.Setenv(key, "")
			}
			t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
			t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", server.URL+"/v1/metrics")
			t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "60000")
			previous := otel.GetMeterProvider()
			defer otel.SetMeterProvider(previous)
			shutdown, err := telemetry.Init(context.Background(), "worker-budget-test")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = shutdown(ctx)
			}()
			fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
			limit, _ := stepbudget.Parse("1")
			b := &stepbudget.Observation{Limit: limit, Used: 1, Responses: 1, Summary: true, Stopped: true, Complete: true}
			fixture.prompter.response = agent.Outcome{Admitted: true, Status: agent.Completed, StepBudget: b}
			outcome := workflow.TurnOutcomeChangeProposalReady
			if exhausted {
				outcome = workflow.TurnOutcomeBlocked
			}
			fixture.outcomes.observation = executionObservation(outcome, store.AgentTurnSucceeded)
			fixture.outcomes.observation.Completion.Outcome, _ = json.Marshal(agent.OutcomeEvidence{Admitted: true, Status: agent.Completed, StepBudget: b, StepBudgetExhausted: exhausted})
			// A failed settlement attempt must not create a duplicate handoff count.
			fixture.store.settleResults = []error{errors.New("temporary settlement failure"), nil}
			fixture.config.CleanupTimeout = time.Millisecond
			processed, err := fixture.worker(t).ProcessNext(context.Background())
			if err != nil || !processed || fixture.store.settled.Outcome != outcome {
				t.Fatalf("settlement changed: %v %v", processed, err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case request := <-received:
				summary, handoff, usage := int64(0), int64(0), float64(0)
				for _, resource := range request.ResourceMetrics {
					for _, scope := range resource.ScopeMetrics {
						for _, m := range scope.Metrics {
							switch m.Name {
							case "omnigrex.agent_turn.budget_summary":
								summary = m.GetSum().DataPoints[0].GetAsInt()
							case "omnigrex.agent_turn.budget_handoffs":
								handoff = m.GetSum().DataPoints[0].GetAsInt()
							case "omnigrex.agent_turn.model_iterations":
								usage = m.GetHistogram().DataPoints[0].GetSum()
							}
						}
					}
				}
				want := int64(0)
				if exhausted {
					want = 1
				}
				if summary != 1 || handoff != want || usage != 1 {
					t.Fatalf("summary=%d handoff=%d usage=%v", summary, handoff, usage)
				}
			default:
				t.Fatal("no budget export")
			}
		})
	}
}
