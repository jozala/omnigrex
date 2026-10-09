package agentturn

import (
	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/session"
)

func runtimeAgentConnection(runtime ExecutionRuntime) session.TurnConnection {
	if runtime == nil {
		return nil
	}
	if capable, ok := runtime.(interface{ AgentConnection() session.TurnConnection }); ok {
		return capable.AgentConnection()
	}
	return nil
}

// Translate neutral observations to the existing durable ending vocabulary.
// This compatibility shim disappears with the ACP surface in Step 17; it does
// not turn uncertain execution into completion or environment-stop evidence.
func legacyPromptEvidence(outcome agent.Outcome, err error) (acp.PromptResponse, error) {
	if err != nil {
		return acp.PromptResponse{}, err
	}
	if !outcome.Admitted {
		return acp.PromptResponse{}, agent.ErrRuntime
	}
	switch outcome.Status {
	case agent.Completed:
		if outcome.LimitReached {
			return acp.PromptResponse{StopReason: acp.StopReasonMaxTokens}, nil
		}
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	case agent.Aborted:
		return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
	case agent.Failed:
		return acp.PromptResponse{}, agent.ErrRuntime
	default:
		return acp.PromptResponse{}, agent.ErrTransport
	}
}
