package agentturn_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/agentturn"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/telemetry"
	"github.com/jozala/omnigrex/internal/workflow"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func operationRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous); _ = provider.Shutdown(context.Background()) })
	return recorder
}

func TestExecutionOperationNestsLaunchAndCleanupAndClassifiesDomainOutcome(t *testing.T) {
	recorder := operationRecorder(t)
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	fixture.outcomes.observation = executionObservation(workflow.TurnOutcomeBlocked, store.AgentTurnSucceeded)
	if processed, err := fixture.worker(t).ProcessNext(context.Background()); err != nil || !processed {
		t.Fatalf("execute: %v", err)
	}
	spans := recorder.Ended()
	if len(spans) != 3 {
		t.Fatalf("expected launch, cleanup, execution; got %d", len(spans))
	}
	execution := spans[2]
	if execution.Name() != string(telemetry.AgentTurnExecute) || execution.Status().Code == codes.Error {
		t.Fatalf("execution: %v", execution)
	}
	for _, child := range spans[:2] {
		if child.Parent().SpanID() != execution.SpanContext().SpanID() {
			t.Fatalf("lost execution parent: %s", child.Name())
		}
	}
	foundOutcome := false
	for _, attr := range execution.Attributes() {
		if string(attr.Key) == "outcome" && attr.Value.AsString() == "domain_outcome" {
			foundOutcome = true
		}
		if strings.Contains(attr.Value.Emit(), "secret") {
			t.Fatal("credential in execution attributes")
		}
	}
	if !foundOutcome {
		t.Fatal("blocked result is not a domain outcome")
	}
}

func TestRecordedReconciliationDoesNotDuplicateItsCallerOperation(t *testing.T) {
	recorder := operationRecorder(t)
	request := outcomeRequest(t, workflow.RoleDeveloper, false)
	reconciler := newOutcomeReconciler(t, &outcomeStore{}, &outcomeGitHub{})
	ctx, operation := telemetry.StartOperation(context.Background(), telemetry.AgentTurnReconcileOutcome)
	_, err := reconciler.Reconcile(ctx, request)
	operation.Finish(&err)
	if len(recorder.Ended()) != 1 {
		t.Fatalf("duplicate reconciliation spans: %d", len(recorder.Ended()))
	}
	if recorder.Ended()[0].Status().Code != codes.Error {
		t.Fatal("missing terminal evidence reported as success")
	}
}

func TestUnresolvedExecutionReportsFailureAfterSuccessfulRecoveryAcknowledgement(t *testing.T) {
	recorder := operationRecorder(t)
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	fixture.store.unsettled = []store.MutationReservation{{ID: "unresolved", State: store.MutationUnknown}}
	if _, err := fixture.worker(t).ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans := recorder.Ended()
	if spans[len(spans)-1].Status().Code != codes.Error {
		t.Fatal("unresolved execution reported as successful")
	}
}

func TestReconciliationCompletionLogsExcludePromptDiagnostics(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(telemetry.NewLogHandler(slog.NewJSONHandler(&output, nil))))
	t.Cleanup(func() { slog.SetDefault(previous) })
	request := outcomeRequest(t, workflow.RoleDeveloper, true)
	request.PromptResponse = nil
	request.PromptError = agentturn.PromptErrorFailure
	request.PromptDiagnostic = "arbitrary agent diagnostic with provider-value-sentinel"
	_, _ = newOutcomeReconciler(t, &outcomeStore{}, &outcomeGitHub{}).Reconcile(context.Background(), request)
	logged := output.String()
	if !strings.Contains(logged, "Agent Turn outcome reconciled") {
		t.Fatal("completion log missing")
	}
	if strings.Contains(logged, "arbitrary agent diagnostic") || strings.Contains(logged, "provider-value-sentinel") {
		t.Fatal("diagnostic leaked into telemetry")
	}
}

func TestEmptyExecutionPollProducesNoOperationSpan(t *testing.T) {
	recorder := operationRecorder(t)
	fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
	fixture.store.acquired = false
	processed, err := fixture.worker(t).ProcessNext(context.Background())
	if err != nil || processed || len(recorder.Started()) != 0 {
		t.Fatalf("empty poll produced telemetry: %v, %v", processed, err)
	}
}
