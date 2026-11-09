package agentevent

import (
	"context"
	"encoding/json"
	"github.com/jozala/omnigrex/internal/stepbudget"
	"time"
)

type Kind string

type Context struct {
	WorkflowID      string
	RetryOfTurnID   string
	AssignmentID    string
	AgentSessionID  string
	TurnID          string
	ExecutionEpoch  uint64
	ControlRevision uint64
}

type AgentEvent struct {
	WorkflowID      string
	RetryOfTurnID   string
	AssignmentID    string
	AgentSessionID  string
	TurnID          string
	ExecutionEpoch  uint64
	ControlRevision uint64
	Kind            Kind
	ObservedAt      time.Time
	Metadata        OperationalMetadata
}

// StepBudgetObservation is the neutral budget evidence shared by live Agent
// Events and terminal outcomes. The alias preserves one validated schema;
// consumers do not translate it or maintain a second counter.
type StepBudgetObservation = stepbudget.Observation

type OperationalMetadata struct {
	ToolObservation       *ToolObservation
	TemporaryFilesystems  []TemporaryFilesystem
	StepBudget            *StepBudgetObservation
	MessageID             string
	ToolCallID            string
	ToolName              string
	ToolKind              string
	Status                string
	FailureClass          string
	ModeID                string
	UsedTokens            *uint64
	ContextSize           *uint64
	CostAmount            *float64
	CostCurrency          string
	AvailableCommandCount *uint64
	Runtime               json.RawMessage
}

// Sink receives bounded operational metadata. Temporary-filesystem observations
// may be delivered concurrently with ordinary events and may finish after the
// originating Prompt returns. Implementations must be concurrency-safe, respect
// context cancellation where possible, and must not relabel late observations
// with a later Turn's context. Critical events retain their serial delivery and
// error semantics; optional filesystem rejection is ignored.
type Sink interface {
	Emit(context.Context, AgentEvent) error
}

type NoopSink struct{}

func (NoopSink) Emit(context.Context, AgentEvent) error {
	return nil
}
