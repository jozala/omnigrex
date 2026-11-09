package telemetry

import (
	"context"
	"math"
	"slices"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var storageBuckets = []float64{0, 1 << 20, 16 << 20, 256 << 20, 1 << 30, 16 << 30, 256 << 30, 1 << 40}
var storageResources = []string{"workspace", "state", "mise", "tool-data", "tmp", "run-pi-agent"}
var storageStatuses = []string{"complete", "partial", "missing", "unavailable"}

// RecordStorage is a sample, not current fleet accounting. Runtime producers
// own tmp/run-pi-agent; the host scanner owns the four persistent categories.
// Only complete/partial traversals may supply allocated bytes; failures must
// pass nil, never a fabricated zero. A complete empty tree may supply zero.
func RecordStorage(ctx context.Context, resource, status string, allocated *int64) {
	if !metricsActive.Load() || !slices.Contains(storageResources, resource) || !slices.Contains(storageStatuses, status) {
		return
	}
	meter := otel.Meter(Scope)
	observations, _ := meter.Int64Counter("omnigrex.storage.observations", metric.WithUnit("{observation}"))
	attrs := metric.WithAttributes(attribute.String("resource", resource), attribute.String("status", status))
	observations.Add(ctx, 1, attrs)
	if allocated == nil || *allocated < 0 || (status != "complete" && status != "partial") {
		return
	}
	usage, _ := meter.Float64Histogram("omnigrex.storage.allocated", metric.WithUnit("By"))
	usage.Record(ctx, float64(*allocated), attrs)
}

// RecordFilesystem samples shared backing capacity once per device per poll,
// without owner/category dimensions. Values are not quotas or additive totals.
// Nil/invalid values omit that sample independently.
func RecordFilesystem(ctx context.Context, capacity, free, available, inodes, freeInodes *float64) {
	if !metricsActive.Load() {
		return
	}
	for i, value := range []*float64{capacity, free, available, inodes, freeInodes} {
		if value == nil || *value < 0 || math.IsInf(*value, 0) || math.IsNaN(*value) {
			continue
		}
		name := []string{"capacity", "free", "available", "inodes", "free_inodes"}[i]
		unit := "By"
		if i >= 3 {
			unit = "{inode}"
		}
		instrument, _ := otel.Meter(Scope).Float64Histogram("omnigrex.storage.filesystem."+name, metric.WithUnit(unit))
		instrument.Record(ctx, *value)
	}
}

// RecordFilesystemUsed records observed block consumption, not directory
// allocation or configured mount size. Like capacity, this is not additive.
func RecordFilesystemUsed(ctx context.Context, used *float64) {
	if !metricsActive.Load() || used == nil || *used < 0 || math.IsNaN(*used) || math.IsInf(*used, 0) {
		return
	}
	instrument, _ := otel.Meter(Scope).Float64Histogram("omnigrex.storage.filesystem.used", metric.WithUnit("By"))
	instrument.Record(ctx, *used)
}
