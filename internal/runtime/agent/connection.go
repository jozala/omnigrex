// Package agent defines runtime-neutral Agent Turn observations.
package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"github.com/jozala/omnigrex/internal/stepbudget"
)

type Status string

const (
	Completed Status = "completed"
	Failed    Status = "failed"
	Aborted   Status = "aborted"
	Rejected  Status = "rejected"
	Unknown   Status = "unknown"
)

// Unknown is an uncertain prompt outcome, never a stop receipt.
type Outcome struct {
	// Admitted records positive evidence that this prompt started. False with
	// Unknown status means admission is unconfirmed, not proven absent.
	Admitted     bool
	Status       Status
	FailureClass string
	// FailureGeneration identifies the credential generation, never a token.
	FailureGeneration string
	LimitReached      bool
	StepBudget        *stepbudget.Observation
	BudgetSummary     string
}

// OutcomeEvidence is the minimal durable controller settlement contract.
// StepBudgetExhausted is set only by reconciliation for incomplete-work Human
// Handoff, never by the runtime or the summary's wording.
type OutcomeEvidence struct {
	Admitted            bool                    `json:"admitted"`
	Status              Status                  `json:"status"`
	LimitReached        bool                    `json:"limit_reached,omitempty"`
	StepBudget          *stepbudget.Observation `json:"step_budget,omitempty"`
	StepBudgetExhausted bool                    `json:"step_budget_exhausted,omitempty"`
}

type Ready struct {
	RuntimeSessionID string
	Capabilities     json.RawMessage
	Disposition      string
}

// SessionIdentity is chosen and reserved before the runtime can create state.
// MustExist forbids replacing an activated Session with an empty conversation.
type SessionIdentity struct {
	AgentSessionID   string `json:"agentSessionId"`
	RuntimeSessionID string `json:"runtimeSessionId"`
	MustExist        bool   `json:"mustExist"`
}

// HistoryCursor is an opaque append-order position scoped to one exact Session.
// Persist it only after the sink acknowledges the corresponding Agent Event.
type HistoryCursor struct {
	RuntimeSessionID string `json:"runtime_session_id"`
	EntryID          string `json:"entry_id"`
}

// Connection does not grant prompt authority. Its caller owns admission/fencing.
// Close only detaches transport; the execution owner must terminate the environment.
type Connection interface {
	Prepare(context.Context) (Ready, error)
	PreparedSessionID() string
	Replay(context.Context, HistoryCursor, agentevent.Sink) (HistoryCursor, error)
	SetAgentEventContext(agentevent.Context)
	Prompt(context.Context, string, agentevent.Sink) (Outcome, error)
	Cancel(context.Context) error
	Close() error
}

var (
	ErrProtocol         = errors.New("agent protocol failure")
	ErrTransport        = errors.New("agent transport lost")
	ErrRuntime          = errors.New("agent runtime rejected operation")
	ErrBusy             = errors.New("agent prompt already active")
	ErrNotReady         = errors.New("agent connection is not prepared")
	ErrBackpressure     = errors.New("agent event consumer exceeded bounded capacity")
	ErrSession          = errors.New("agent session identity mismatch")
	ErrCleanup          = errors.New("agent transport cleanup did not complete")
	ErrDeadlineRequired = errors.New("agent prompt requires a caller deadline")
)
