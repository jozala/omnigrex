// Package agent defines runtime-neutral Agent Turn observations.
package agent

import (
	"context"
	"errors"

	"github.com/jozala/omnigrex/internal/runtime/agentevent"
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
}

type Ready struct{ RuntimeSessionID string }

// Connection does not grant prompt authority. Its caller owns admission/fencing.
// Close only detaches transport; the execution owner must terminate the environment.
type Connection interface {
	Prepare(context.Context) (Ready, error)
	Prompt(context.Context, string, agentevent.Sink) (Outcome, error)
	Cancel(context.Context) error
	Close() error
}

var (
	ErrProtocol     = errors.New("agent protocol failure")
	ErrTransport    = errors.New("agent transport lost")
	ErrRuntime      = errors.New("agent runtime rejected operation")
	ErrBusy         = errors.New("agent prompt already active")
	ErrNotReady     = errors.New("agent connection is not prepared")
	ErrBackpressure = errors.New("agent event consumer exceeded bounded capacity")
	ErrSession      = errors.New("agent session identity mismatch")
	ErrCleanup      = errors.New("agent transport cleanup did not complete")
)
