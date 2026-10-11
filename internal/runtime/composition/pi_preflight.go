package composition

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jozala/omnigrex/internal/agentinstructions"
	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/pi"
	"github.com/jozala/omnigrex/internal/workflow"
)

func ValidatePiProfile(profile agentprofile.Profile, policy role.Policy, operator agentinstructions.Operator, stage workflow.StageDefinition, purpose workflow.TurnPurpose, endpoint agent.MCPEndpoint, credentials []byte) error {
	_, err := pi.Render(profile, policy, operator, stage, purpose, endpoint, credentials)
	return err
}

// ConnectPiDiagnostic uses the production runner and disposable MCP authority.
// The caller prepares only, never submits a prompt or supplies deployment keys.
func ConnectPiDiagnostic(ctx context.Context, transport io.ReadWriteCloser, endpoint agent.MCPEndpoint, selection agent.ModelSelection) (agent.Connection, error) {
	id := uuid.NewString()
	provider, _, _ := strings.Cut(selection.Model, "/")
	auth, err := json.Marshal(map[string]any{provider: map[string]string{"type": "api_key", "key": "doctor-fixture-only"}})
	if err != nil {
		return nil, err
	}
	return pi.Start(ctx, transport, pi.Configuration{
		Selection: selection, Tools: []string{}, Instructions: "Initialize the diagnostic only; do not submit a model prompt.",
		MCP: endpoint, MCPTools: []string{"get_issue"}, Auth: auth,
		Session: &agent.SessionIdentity{AgentSessionID: id, RuntimeSessionID: id},
	}, pi.Options{RequestTimeout: 10 * time.Second})
}
