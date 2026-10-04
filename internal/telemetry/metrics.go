package telemetry

import (
	"context"
	"slices"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

const ObservationTimeout = 5 * time.Second

// These stable allowlists are shared with the read-only aggregate observer.
// Adding a category requires updating the series budget and operator contract.
var workflowStates = [...]string{"DORMANT", "DEVELOPING", "REVIEWING", "PR_READY", "NEEDS_HUMAN", "CLOSING", "CLOSED"}
var queueCategories = [...]string{"preparation", "execution", "runtime_cleanup", "mutation_recovery", "corroboration", "webhooks", "historical_events", "label_provisioning"}
var operationNames = [...]OperationName{AgentTurnPrepare, AgentTurnExecute, RuntimeProcessLaunch, AgentTurnReconcileOutcome, MCPToolExecute, WebhookProcess, MutationRecover, RuntimeProcessCleanup}
var durationBuckets = []float64{0.1, 0.5, 1, 5, 15, 60, 300, 900, 3600, 7200}
var metricsActive atomic.Bool

// DurableState is one all-or-nothing database snapshot. Maps are projected onto
// fixed allowlists before export; IDs and unknown categories cannot become labels.
type DurableState struct {
	Workflows               map[string]int64
	ActiveTurns             int64
	Pending                 map[string]PendingWork
	UnresolvedMutations     int64
	OldestUnresolvedSeconds float64
}

type PendingWork struct {
	Count         int64
	OldestSeconds float64
}

func applicationMetricView(instrument sdkmetric.Instrument) (sdkmetric.Stream, bool) {
	stream := sdkmetric.Stream{Name: instrument.Name, Aggregation: sdkmetric.AggregationDrop{}}
	if instrument.Scope.Name != Scope {
		return stream, true
	}
	switch instrument.Name {
	case "omnigrex.operation.duration":
		stream.Aggregation = sdkmetric.AggregationExplicitBucketHistogram{Boundaries: durationBuckets, NoMinMax: true}
		stream.AttributeFilter = attribute.NewAllowKeysFilter("operation")
	case "omnigrex.operation.attempts":
		stream.Aggregation = sdkmetric.AggregationDefault{}
		stream.AttributeFilter = attribute.NewAllowKeysFilter("operation", "outcome")
	case "omnigrex.workflow.count":
		stream.Aggregation = sdkmetric.AggregationDefault{}
		stream.AttributeFilter = attribute.NewAllowKeysFilter("state")
	case "omnigrex.work.pending", "omnigrex.work.oldest_pending_age":
		stream.Aggregation = sdkmetric.AggregationDefault{}
		stream.AttributeFilter = attribute.NewAllowKeysFilter("queue_category")
	case "omnigrex.agent_turn.active", "omnigrex.mutation.unresolved", "omnigrex.mutation.oldest_unresolved_age", "omnigrex.observation.failures":
		stream.Aggregation = sdkmetric.AggregationDefault{}
		stream.AttributeFilter = attribute.NewAllowKeysFilter()
	}
	return stream, true
}

func recordOperation(ctx context.Context, name OperationName, outcome Outcome, duration time.Duration) {
	if !metricsActive.Load() || !slices.Contains(operationNames[:], name) {
		return
	}
	switch outcome {
	case Success, DomainOutcome, Failure, Cancelled, Timeout:
	default:
		return
	}
	meter := otel.Meter(Scope)
	// Names and options are constants validated by the SDK; creation cannot fail.
	attempts, _ := meter.Int64Counter("omnigrex.operation.attempts", metric.WithUnit("{attempt}"))
	elapsed, _ := meter.Float64Histogram("omnigrex.operation.duration", metric.WithUnit("s"))
	attempts.Add(ctx, 1, metric.WithAttributes(attribute.String("operation", string(name)), attribute.String("outcome", string(outcome))))
	elapsed.Record(ctx, duration.Seconds(), metric.WithAttributes(attribute.String("operation", string(name))))
}

// RegisterStateObserver installs a single callback for all durable instruments.
// Its cleanup must run after provider shutdown but before closing the database.
func RegisterStateObserver(observe func(context.Context) (DurableState, error)) (func(), error) {
	if !metricsActive.Load() {
		return func() {}, nil
	}
	meter := otel.Meter(Scope)
	workflows, _ := meter.Int64ObservableGauge("omnigrex.workflow.count", metric.WithUnit("{workflow}"))
	active, _ := meter.Int64ObservableGauge("omnigrex.agent_turn.active", metric.WithUnit("{turn}"))
	pending, _ := meter.Int64ObservableGauge("omnigrex.work.pending", metric.WithUnit("{item}"))
	age, _ := meter.Float64ObservableGauge("omnigrex.work.oldest_pending_age", metric.WithUnit("s"))
	mutations, _ := meter.Int64ObservableGauge("omnigrex.mutation.unresolved", metric.WithUnit("{mutation}"))
	mutationAge, _ := meter.Float64ObservableGauge("omnigrex.mutation.oldest_unresolved_age", metric.WithUnit("s"))
	failures, _ := meter.Int64Counter("omnigrex.observation.failures", metric.WithUnit("{collection}"))
	failures.Add(context.Background(), 0)
	gate := make(chan struct{}, 1)
	registration, err := meter.RegisterCallback(func(ctx context.Context, observer metric.Observer) error {
		ctx, cancel := context.WithTimeout(ctx, ObservationTimeout)
		defer cancel()
		select {
		case gate <- struct{}{}:
			defer func() { <-gate }()
		case <-ctx.Done():
			failures.Add(ctx, 1)
			return nil
		}
		state, err := observe(ctx)
		if err != nil {
			failures.Add(ctx, 1)
			return nil
		}
		for _, name := range workflowStates {
			observer.ObserveInt64(workflows, state.Workflows[name], metric.WithAttributes(attribute.String("state", name)))
		}
		observer.ObserveInt64(active, state.ActiveTurns)
		for _, name := range queueCategories {
			value := state.Pending[name]
			attributes := metric.WithAttributes(attribute.String("queue_category", name))
			observer.ObserveInt64(pending, value.Count, attributes)
			observer.ObserveFloat64(age, max(0, value.OldestSeconds), attributes)
		}
		observer.ObserveInt64(mutations, state.UnresolvedMutations)
		observer.ObserveFloat64(mutationAge, max(0, state.OldestUnresolvedSeconds))
		return nil
	}, workflows, active, pending, age, mutations, mutationAge)
	if err != nil {
		return nil, err
	}
	return func() { _ = registration.Unregister() }, nil
}
