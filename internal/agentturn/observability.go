package agentturn

import (
	"context"
	"errors"

	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/telemetry"
	"github.com/jozala/omnigrex/internal/workflow"
	"go.opentelemetry.io/otel/attribute"
)

func jobAttributes(lease store.JobLease) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("workflow_id", lease.WorkflowID),
		attribute.String("agent_participant_id", lease.AgentParticipantID),
		attribute.String("agent_session_id", lease.AgentSessionID),
		attribute.String("agent_turn_id", lease.AgentTurnID),
	}
}

func executionAttributes(execution store.AgentTurnExecutionContext) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("workflow_id", execution.WorkflowID),
		attribute.String("agent_participant_id", execution.Turn.AgentParticipantID),
		attribute.String("agent_session_id", execution.Session.ID),
		attribute.String("agent_turn_id", execution.Turn.ID),
		attribute.String("role", string(execution.Assignment.Role)),
		attribute.String("stage", string(execution.Turn.Stage)),
	}
}

func finishOperation(ctx context.Context, operation *telemetry.Operation, err *error) {
	if errors.Is(*err, store.ErrAgentTurnFenceLost) || errors.Is(*err, store.ErrJobLeaseLost) ||
		errors.Is(*err, store.ErrAgentTurnRecoveryFenceLost) || errors.Is(*err, store.ErrAgentTurnPreparationFenceLost) {
		telemetry.SetOutcome(ctx, telemetry.Cancelled, "fence_lost")
	} else if errors.Is(*err, ErrRuntimeOOMKilled) {
		telemetry.SetOutcome(ctx, telemetry.Failure, "runtime_oom_killed")
	} else if errors.Is(*err, context.DeadlineExceeded) {
		telemetry.SetOutcome(ctx, telemetry.Timeout, "deadline_exceeded")
	} else if errors.Is(*err, context.Canceled) {
		telemetry.SetOutcome(ctx, telemetry.Cancelled, "cancelled")
	}
	operation.Finish(err)
}

func observeSettlement(ctx context.Context, observation store.AgentTurnSettlementObservation) {
	telemetry.AddAttributes(ctx, attribute.String("status", string(observation.Completion.Status)), attribute.String("domain_outcome", string(observation.Outcome)))
	switch observation.Completion.Status {
	case store.AgentTurnTimedOut:
		telemetry.SetOutcome(ctx, telemetry.Timeout, "deadline_exceeded")
	case store.AgentTurnInterrupted:
		telemetry.SetOutcome(ctx, telemetry.Cancelled, "cancelled")
	default:
		switch observation.Outcome {
		case workflow.TurnOutcomeInfrastructureFailed:
			telemetry.SetOutcome(ctx, telemetry.Failure, "outcome_unavailable")
		case workflow.TurnOutcomeBlocked:
			telemetry.SetOutcome(ctx, telemetry.DomainOutcome, "")
		}
	}
}

func startReconciliation(ctx context.Context, request OutcomeReconciliation) (context.Context, func(store.AgentTurnSettlementObservation, *error)) {
	var operation *telemetry.Operation
	if !telemetry.OperationIs(ctx, telemetry.AgentTurnReconcileOutcome) {
		ctx, operation = telemetry.StartOperation(ctx, telemetry.AgentTurnReconcileOutcome, executionAttributes(request.Execution)...)
	} else {
		telemetry.AddAttributes(ctx, executionAttributes(request.Execution)...)
	}
	return ctx, func(observation store.AgentTurnSettlementObservation, err *error) {
		observeSettlement(ctx, observation)
		if operation != nil {
			finishOperation(ctx, operation, err)
		}
	}
}
