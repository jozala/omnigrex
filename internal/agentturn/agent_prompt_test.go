package agentturn_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/agentturn"
	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestExecutionWorkerPreservesNeutralPromptEndings(t *testing.T) {
	for _, tc := range []struct {
		name           string
		outcome        agent.Outcome
		reason         acp.StopReason
		classification agentturn.PromptErrorClassification
	}{
		{"settled", agent.Outcome{Admitted: true, Status: agent.Completed}, acp.StopReasonEndTurn, ""},
		{"token limit", agent.Outcome{Admitted: true, Status: agent.Completed, LimitReached: true}, acp.StopReasonMaxTokens, ""},
		{"abort", agent.Outcome{Admitted: true, Status: agent.Aborted}, acp.StopReasonCancelled, ""},
		{"provider failure", agent.Outcome{Admitted: true, Status: agent.Failed}, "", agentturn.PromptErrorFailure},
		{"lost settlement", agent.Outcome{Admitted: true, Status: agent.Unknown}, "", agentturn.PromptErrorFailure},
		{"unconfirmed admission", agent.Outcome{Status: agent.Completed}, "", agentturn.PromptErrorFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newExecutionWorkerFixture(t, workflow.RoleDeveloper)
			connection := &neutralPromptFixture{outcome: tc.outcome}
			fixture.runtime.agent = connection
			fixture.runtime.client = nil
			fixture.prompter.err = errors.New("legacy protocol must not be called")
			processed, _ := fixture.worker(t).ProcessNext(t.Context())
			if !processed || !strings.Contains(connection.text, "agent_turn_id") {
				t.Fatal("neutral runtime did not receive the structured prompt")
			}
			request := fixture.outcomes.request
			if request.PromptError != tc.classification {
				t.Fatalf("classification: %s", request.PromptError)
			}
			if tc.reason == "" {
				if request.PromptResponse != nil {
					t.Fatal("uncertain/failure ending became completion")
				}
			} else if request.PromptResponse == nil || request.PromptResponse.StopReason != tc.reason {
				t.Fatalf("ending: %+v", request.PromptResponse)
			}
		})
	}
}

type neutralPromptFixture struct {
	outcome agent.Outcome
	text    string
}

func (f *neutralPromptFixture) Prompt(_ context.Context, _ store.AgentTurnLease, _ store.AgentSession, text string, _ agentevent.Sink) (agent.Outcome, error) {
	f.text = text
	return f.outcome, nil
}
func (*neutralPromptFixture) Close() error { return nil }
