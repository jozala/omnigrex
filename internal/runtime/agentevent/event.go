package agentevent

import (
	"context"
	"encoding/json"
	"time"
)

type Kind string

type Context struct {
	AssignmentID    string
	AgentSessionID  string
	ACPSessionID    string
	TurnID          string
	ExecutionEpoch  uint64
	ControlRevision uint64
}

type AgentEvent struct {
	AssignmentID    string
	AgentSessionID  string
	TurnID          string
	ExecutionEpoch  uint64
	ControlRevision uint64
	Kind            Kind
	ObservedAt      time.Time
	Metadata        OperationalMetadata
}

type OperationalMetadata struct {
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

type Sink interface {
	Emit(context.Context, AgentEvent) error
}

type NoopSink struct{}

func (NoopSink) Emit(context.Context, AgentEvent) error {
	return nil
}
