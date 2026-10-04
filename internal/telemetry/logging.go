package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// NewLogHandler enriches records using only the supplied span context. Derived
// handlers retain normal slog grouping, attribute binding, and level behavior.
func NewLogHandler(handler slog.Handler) slog.Handler { return &logHandler{handler} }

type logHandler struct{ slog.Handler }

func (h *logHandler) Handle(ctx context.Context, record slog.Record) error {
	span := trace.SpanContextFromContext(ctx)
	if span.IsValid() {
		record = record.Clone()
		record.AddAttrs(slog.String("trace_id", span.TraceID().String()), slog.String("span_id", span.SpanID().String()))
	}
	return h.Handler.Handle(ctx, record)
}

func (h *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logHandler{h.Handler.WithAttrs(attrs)}
}

func (h *logHandler) WithGroup(name string) slog.Handler {
	return &logHandler{h.Handler.WithGroup(name)}
}
