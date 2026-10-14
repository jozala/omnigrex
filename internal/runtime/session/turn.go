package session

import (
	"context"
	"encoding/json"
	"io"

	"github.com/jozala/omnigrex/internal/agentinstructions"
	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

// TurnAdapter is selected by composition, never by Workflow or backend policy.
type TurnAdapter interface {
	BuildProcess(profile.Profile, execution.Key, string, int64, map[string]string) (execution.Request, error)
	Prepare(context.Context, TurnPreparation) (TurnConnection, error)
}

type TurnPreparation struct {
	Lease       store.AgentTurnLease
	Session     store.AgentSession
	State       execution.StateRef
	Transport   io.ReadWriteCloser
	Profile     json.RawMessage
	ProfileName string
	Policy      role.Policy
	Stage       workflow.StageDefinition
	Purpose     workflow.TurnPurpose
	Operator    agentinstructions.Operator
	MCP         agent.MCPEndpoint
	Credentials []byte
}

type TurnConnection interface {
	Prompt(context.Context, store.AgentTurnLease, store.AgentSession, string, agentevent.Sink) (agent.Outcome, error)
	Close() error
}
