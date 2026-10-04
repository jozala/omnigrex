package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const Scope = "github.com/jozala/omnigrex/operations"

type OperationName string

const (
	AgentTurnPrepare          OperationName = "agent_turn.prepare"
	AgentTurnExecute          OperationName = "agent_turn.execute"
	RuntimeProcessLaunch      OperationName = "runtime_process.launch"
	AgentTurnReconcileOutcome OperationName = "agent_turn.reconcile_outcome"
	MCPToolExecute            OperationName = "mcp.tool.execute"
	WebhookProcess            OperationName = "webhook.process"
	MutationRecover           OperationName = "mutation.recover"
	RuntimeProcessCleanup     OperationName = "runtime_process.cleanup"
)

type Outcome string

const (
	Success       Outcome = "success"
	DomainOutcome Outcome = "domain_outcome"
	Failure       Outcome = "failure"
	Cancelled     Outcome = "cancelled"
	Timeout       Outcome = "timeout"
)

type operationKey struct{}

// Operation records a logical attempt. Call Finish exactly once, after all
// admitted work and durable acknowledgement finish, even if HTTP has returned.
type Operation struct {
	mu       sync.Mutex
	ctx      context.Context
	span     trace.Span
	name     OperationName
	started  time.Time
	attrs    []attribute.KeyValue
	outcome  Outcome
	category string
}

func StartOperation(ctx context.Context, name OperationName, attrs ...attribute.KeyValue) (context.Context, *Operation) {
	if parent, ok := ctx.Value(operationKey{}).(*Operation); ok {
		parent.mu.Lock()
		attrs = append(append([]attribute.KeyValue(nil), parent.attrs...), attrs...)
		parent.mu.Unlock()
	}
	attrs = nonemptyAttributes(attrs)
	ctx, span := otel.Tracer(Scope).Start(ctx, string(name), trace.WithAttributes(attrs...))
	op := &Operation{span: span, name: name, started: time.Now(), attrs: attrs, outcome: Success}
	ctx = context.WithValue(ctx, operationKey{}, op)
	op.ctx = ctx
	slog.InfoContext(ctx, "operation started", op.logAttrs()...)
	return ctx, op
}

// OperationIs allows a shared reconciliation boundary to join its caller-owned
// attempt without creating duplicate spans for the same logical operation.
func OperationIs(ctx context.Context, name OperationName) bool {
	op, ok := ctx.Value(operationKey{}).(*Operation)
	return ok && op.name == name
}

// SetOutcome accepts only code-defined categories, never external error text.
func SetOutcome(ctx context.Context, outcome Outcome, category string) {
	if op, ok := ctx.Value(operationKey{}).(*Operation); ok {
		op.mu.Lock()
		defer op.mu.Unlock()
		op.outcome, op.category = outcome, category
	}
}

func AddAttributes(ctx context.Context, attrs ...attribute.KeyValue) {
	attrs = nonemptyAttributes(attrs)
	if op, ok := ctx.Value(operationKey{}).(*Operation); ok {
		op.mu.Lock()
		defer op.mu.Unlock()
		merged := attribute.NewSet(append(op.attrs, attrs...)...)
		op.attrs = merged.ToSlice()
		op.span.SetAttributes(attrs...)
	}
}

func nonemptyAttributes(attrs []attribute.KeyValue) []attribute.KeyValue {
	result := make([]attribute.KeyValue, 0, len(attrs))
	for _, attr := range attrs {
		if attr.Value.Type() != attribute.STRING || attr.Value.AsString() != "" {
			result = append(result, attr)
		}
	}
	unique := attribute.NewSet(result...)
	return unique.ToSlice()
}

func (op *Operation) Finish(err *error) {
	op.mu.Lock()
	defer op.mu.Unlock()
	if *err != nil {
		switch {
		case op.outcome == Failure || op.outcome == Cancelled || op.outcome == Timeout:
			// Explicit domain classifications take precedence over secondary
			// cancellation/deadline errors joined during cleanup.
		case errors.Is(*err, context.DeadlineExceeded):
			op.outcome, op.category = Timeout, "deadline_exceeded"
		case errors.Is(*err, context.Canceled):
			op.outcome, op.category = Cancelled, "cancelled"
		case op.outcome == Success || op.outcome == DomainOutcome:
			op.outcome, op.category = Failure, "operation_failed"
		}
		*err = operationError{error: *err, ctx: op.ctx}
	}
	op.span.SetAttributes(attribute.String("outcome", string(op.outcome)))
	if op.category != "" {
		op.span.SetAttributes(attribute.String("failure_category", op.category))
	}
	if op.outcome == Failure || op.outcome == Timeout {
		op.span.SetStatus(codes.Error, op.category)
	}
	attrs := append(op.logAttrs(), "outcome", string(op.outcome), "duration_seconds", time.Since(op.started).Seconds())
	if op.category != "" {
		attrs = append(attrs, "failure_category", op.category)
	}
	message := "operation finished"
	if op.name == AgentTurnReconcileOutcome {
		message = "Agent Turn outcome reconciled"
	}
	slog.InfoContext(op.ctx, message, attrs...)
	recordOperation(op.ctx, op.name, op.outcome, time.Since(op.started))
	op.span.End()
}

func (op *Operation) logAttrs() []any {
	attrs := []any{"operation", string(op.name)}
	for _, attr := range op.attrs {
		attrs = append(attrs, string(attr.Key), attr.Value.AsInterface())
	}
	return attrs
}

// CopyContext preserves observability while leaving the destination's lifetime
// and cancellation cause unchanged. It does not copy arbitrary request values.
func CopyContext(destination, source context.Context) context.Context {
	destination = trace.ContextWithSpan(destination, trace.SpanFromContext(source))
	if op, ok := source.Value(operationKey{}).(*Operation); ok {
		destination = context.WithValue(destination, operationKey{}, op)
	}
	return destination
}

type operationError struct {
	error
	ctx context.Context
}

func (err operationError) Unwrap() error { return err.error }

// ErrorContext lets existing sanitized worker error callbacks retain the
// operation identity after ProcessNext returns, without changing error matching.
func ErrorContext(fallback context.Context, err error) context.Context {
	var observed operationError
	if errors.As(err, &observed) {
		return observed.ctx
	}
	return fallback
}
