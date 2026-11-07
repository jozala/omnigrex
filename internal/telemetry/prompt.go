package telemetry

import (
	"context"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"time"
)

// RecordPromptDuration measures only the Prompt call; a caller return without
// terminal evidence is not evidence that guest execution stopped.
func RecordPromptDuration(ctx context.Context, duration time.Duration, confirmed bool) {
	if !metricsActive.Load() || duration < 0 {
		return
	}
	boundary := "caller_return"
	if confirmed {
		boundary = "runtime_terminal"
	}
	histogram, _ := otel.Meter(Scope).Float64Histogram("omnigrex.agent_turn.prompt_duration", metric.WithUnit("s"))
	histogram.Record(ctx, duration.Seconds(), metric.WithAttributes(attribute.String("boundary", boundary)))
}
