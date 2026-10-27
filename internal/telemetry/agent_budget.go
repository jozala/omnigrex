package telemetry

import (
	"context"
	"sync"

	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"github.com/jozala/omnigrex/internal/stepbudget"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// AgentTurnObserver consumes live neutral events for one prompt. It retains
// constant space and records SDK aggregates, never waiting for network export.
// Deduplication is local to this lifetime, not a durable exactly-once guarantee.
type AgentTurnObserver struct {
	downstream        agentevent.Sink
	role              string
	mu                sync.Mutex
	summary, finished bool
}

func NewAgentTurnObserver(role string, downstream agentevent.Sink) *AgentTurnObserver {
	return &AgentTurnObserver{role: budgetRole(role), downstream: downstream}
}
func budgetRole(role string) string {
	switch role {
	case "DEVELOPER", "REVIEWER":
		return role
	default:
		return "unknown"
	}
}
func (o *AgentTurnObserver) Emit(ctx context.Context, event agentevent.AgentEvent) error {
	if event.Kind == "step_budget" && event.Metadata.StepBudget != nil && event.Metadata.StepBudget.Valid() {
		o.observeSummary(ctx, *event.Metadata.StepBudget)
	}
	if o.downstream != nil {
		return o.downstream.Emit(ctx, event)
	}
	return nil
}
func (o *AgentTurnObserver) observeSummary(ctx context.Context, b stepbudget.Observation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.finished && b.Summary && !o.summary {
		o.summary = true
		budgetSignal(ctx, "omnigrex.agent_turn.budget_summary", o.role)
	}
}

// Finish uses the adapter's critical snapshot, not the droppable live queue.
// Nil/invalid evidence is unknown usage and never becomes a complete zero.
func (o *AgentTurnObserver) Finish(ctx context.Context, b *stepbudget.Observation, termination string) {
	if b != nil && b.Valid() {
		o.observeSummary(ctx, *b)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished {
		return
	}
	o.finished = true
	if !metricsActive.Load() {
		return
	}
	switch termination {
	case "completed", "failed", "cancelled", "transport":
	default:
		termination = "unknown"
	}
	mode, completeness := "unknown", "unknown"
	if b != nil && b.Valid() {
		mode, completeness = "unlimited", "partial"
		if b.Limit.Finite() {
			mode = "finite"
		}
		if b.Complete {
			completeness = "complete"
		}
	}
	dimensions := []attribute.KeyValue{attribute.String("role", o.role), attribute.String("budget_mode", mode), attribute.String("completeness", completeness)}
	meter := otel.Meter(Scope)
	turns, _ := meter.Int64Counter("omnigrex.agent_turn.budget_observations", metric.WithUnit("{turn}"))
	turns.Add(ctx, 1, metric.WithAttributes(append(dimensions, attribute.String("termination", termination))...))
	if b == nil || !b.Valid() {
		return
	}
	attrs := metric.WithAttributes(dimensions...)
	used, _ := meter.Float64Histogram("omnigrex.agent_turn.model_iterations", metric.WithUnit("{iteration}"))
	used.Record(ctx, float64(b.Used), attrs)
	if b.Limit.Finite() {
		limit, _ := meter.Float64Histogram("omnigrex.agent_turn.step_limit", metric.WithUnit("{iteration}"))
		limit.Record(ctx, float64(b.Limit.Steps()), attrs)
		utilization, _ := meter.Float64Histogram("omnigrex.agent_turn.budget_utilization", metric.WithUnit("1"))
		utilization.Record(ctx, float64(b.Used)/float64(b.Limit.Steps()), attrs)
	}
}
func budgetSignal(ctx context.Context, name, role string) {
	if !metricsActive.Load() {
		return
	}
	counter, _ := otel.Meter(Scope).Int64Counter(name, metric.WithUnit("{turn}"))
	counter.Add(ctx, 1, metric.WithAttributes(attribute.String("role", role)))
}

// RecordBudgetHandoff records successfully settled incomplete-work handoffs,
// never summary entry or speculative outcome reconciliation.
func RecordBudgetHandoff(ctx context.Context, role string) {
	budgetSignal(ctx, "omnigrex.agent_turn.budget_handoffs", budgetRole(role))
}
