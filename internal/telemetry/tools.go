package telemetry

import (
	"context"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/agentevent"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// KnownMCPToolName checks the closed server/client vocabulary, kept in sync
// with the capability catalog by tests. It never expands the vocabulary.
func KnownMCPToolName(name string) bool { return agentevent.KnownMCPToolName(name) }

// RecordTool records one observed terminal call, not an admission or paired event
// ledger. Layers are independent populations: mcp_server, mcp_client, native.
// Missing duration omits the histogram, including for interrupted native calls.
// Unknown names/layers/outcomes are discarded, never exported as labels.
func RecordTool(ctx context.Context, layer, name string, outcome Outcome, duration *time.Duration) {
	if !metricsActive.Load() {
		return
	}
	switch layer {
	case "mcp_server", "mcp_client":
		if !KnownMCPToolName(name) {
			return
		}
	case "native":
		if !agentevent.KnownNativeToolName(name) {
			return
		}
	default:
		return
	}
	switch outcome {
	case Success, DomainOutcome, Failure, Cancelled, Timeout:
	default:
		return
	}
	meter := otel.Meter(Scope)
	calls, _ := meter.Int64Counter("omnigrex.tool.calls", metric.WithUnit("{call}"))
	elapsed, _ := meter.Float64Histogram("omnigrex.tool.duration", metric.WithUnit("s"))
	attrs := []attribute.KeyValue{attribute.String("layer", layer), attribute.String("tool", name)}
	calls.Add(ctx, 1, metric.WithAttributes(append(attrs, attribute.String("outcome", string(outcome)))...))
	if duration != nil && *duration >= 0 {
		elapsed.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
	}
}
