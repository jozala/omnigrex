package agentturn

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jozala/omnigrex/internal/providerauth"
	"github.com/jozala/omnigrex/internal/runtime/agent"
)

// An admitted failed model request is not a rejected runtime operation. Keep
// this generic failure distinct from subscription prerequisite sentinels: an
// observed summary category is diagnostic evidence, not authority to reroute it.
var errProviderRequest = errors.New("provider_failure: model request failed")

// Only positively admitted completion or abort evidence is a terminal response.
// Uncertain admission and settlement remain failures and never prove execution stopped.
func normalizedPromptEvidence(outcome agent.Outcome, err error) (agent.Outcome, error) {
	if err != nil {
		return agent.Outcome{}, err
	}
	switch outcome.Status {
	case agent.Completed, agent.Aborted:
		if !outcome.Admitted {
			return agent.Outcome{}, agent.ErrRuntime
		}
		return outcome, nil
	case agent.Failed, agent.Rejected:
		if outcome.Admitted && outcome.Status == agent.Failed {
			switch outcome.FailureClass {
			case providerauth.QuotaFailure:
				return agent.Outcome{}, promptProviderError(outcome, providerauth.ErrQuota)
			case providerauth.AuthFailure:
				return agent.Outcome{}, promptProviderError(outcome, providerauth.ErrAuthentication)
			case providerauth.TemporaryFailure:
				return agent.Outcome{}, promptProviderError(outcome, providerauth.ErrRequestTemporary)
			case providerauth.ConfigurationFailure:
				return agent.Outcome{}, promptProviderError(outcome, providerauth.ErrConfiguration)
			}
			return agent.Outcome{}, promptProviderError(outcome, errProviderRequest)
		}
		return agent.Outcome{}, agent.ErrRuntime
	case agent.Unknown:
		return agent.Outcome{}, agent.ErrTransport
	default:
		return agent.Outcome{}, agent.ErrProtocol
	}
}

func promptProviderError(outcome agent.Outcome, cause error) error {
	if outcome.FailureDiagnostic == nil {
		return cause
	}
	raw, _ := json.Marshal(outcome.FailureDiagnostic)
	return fmt.Errorf("%w (%s)", cause, agent.ParseProviderDiagnostic(raw).Summary())
}
