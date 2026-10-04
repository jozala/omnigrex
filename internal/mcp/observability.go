package mcp

import (
	"context"
	"errors"

	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
)

func startToolOperation(ctx context.Context, invocation Invocation) (context.Context, *telemetry.Operation) {
	return telemetry.StartOperation(ctx, telemetry.MCPToolExecute,
		attribute.String("workflow_id", invocation.Scope.WorkflowID),
		attribute.String("agent_participant_id", invocation.lease.AgentParticipantID),
		attribute.String("agent_session_id", invocation.Scope.AgentSessionID),
		attribute.String("agent_turn_id", invocation.Scope.AgentTurnID),
		attribute.String("role", string(invocation.Scope.Role)),
		attribute.String("stage", string(invocation.lease.Stage)),
		attribute.String("tool_name", invocation.Name))
}

func observeToolError(ctx context.Context, err error) {
	switch {
	case errors.Is(err, store.ErrAgentTurnFenceLost), errors.Is(err, store.ErrJobLeaseLost):
		telemetry.SetOutcome(ctx, telemetry.Cancelled, "fence_lost")
	case errors.Is(err, context.DeadlineExceeded):
		telemetry.SetOutcome(ctx, telemetry.Timeout, "deadline_exceeded")
	case errors.Is(err, context.Canceled):
		telemetry.SetOutcome(ctx, telemetry.Cancelled, "cancelled")
	}
}

func toolSucceeded(ctx context.Context, name string) {
	outcome := telemetry.Success
	if name == ToolReportBlocked {
		outcome = telemetry.DomainOutcome
	}
	telemetry.SetOutcome(ctx, outcome, "")
}
