package agentturn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestTerminalCorroborationBackoffIsBounded(t *testing.T) {
	for _, test := range []struct {
		attempt int
		want    time.Duration
	}{
		{1, 5 * time.Second}, {2, 10 * time.Second},
		{3, 20 * time.Second}, {6, 2 * time.Minute},
		{1000000, 2 * time.Minute},
	} {
		if got := terminalCorroborationBackoff(test.attempt); got != test.want {
			t.Errorf("attempt %d backoff = %s, want %s", test.attempt, got, test.want)
		}
	}
}

func TestTerminalCorroborationContextConflictHandsOffExactSource(t *testing.T) {
	for _, kind := range []string{store.VerifyTerminalIntentJobKind, store.RevalidateTerminalIntentJobKind} {
		t.Run(kind, func(t *testing.T) {
			database := newContextErrorCorroborationStore(kind, fmt.Errorf("read persisted checkpoint: %w", store.ErrTerminalCorroborationConflict))
			worker := &TerminalCorroborationWorker{store: database, config: TerminalCorroborationWorkerConfig{ClaimOwner: "verifier", Window: time.Hour, LeaseDuration: time.Minute}}
			processed, err := worker.ProcessOne(t.Context())
			if !processed || err != nil {
				t.Fatalf("ProcessOne = %t, %v", processed, err)
			}
			if database.handoffKind != kind || database.handoffReason != workflow.ReasonTerminalCorroborationPrerequisite || database.handoffCode != "terminal_evidence_conflict" {
				t.Fatalf("incompatible checkpoint did not reach prerequisite handoff: kind=%q, reason=%q, code=%q", database.handoffKind, database.handoffReason, database.handoffCode)
			}
			if !reflect.DeepEqual(database.resultLease, database.lease) || database.completedSource != database.checkpoint.SourceInvocationID {
				t.Fatalf("handoff lost exact source fence: lease=%#v, source=%q", database.resultLease, database.completedSource)
			}
			if database.lease.Status != store.JobLeased || database.completedSource == "" {
				t.Fatal("test did not exercise a valid leased source checkpoint")
			}
		})
	}
}

func TestTerminalCorroborationContextTransportErrorRemainsUnreadable(t *testing.T) {
	for _, kind := range []string{store.VerifyTerminalIntentJobKind, store.RevalidateTerminalIntentJobKind} {
		t.Run(kind, func(t *testing.T) {
			transportErr := errors.New("database transport unavailable")
			database := newContextErrorCorroborationStore(kind, fmt.Errorf("read persisted checkpoint: %w", transportErr))
			worker := &TerminalCorroborationWorker{store: database, config: TerminalCorroborationWorkerConfig{ClaimOwner: "verifier", Window: time.Hour, LeaseDuration: time.Minute}}
			processed, err := worker.ProcessOne(t.Context())
			if !processed || !errors.Is(err, transportErr) {
				t.Fatalf("ProcessOne = %t, %v, want unreadable context error", processed, err)
			}
			if database.handoffKind != "" || database.completedSource != "" {
				t.Fatalf("unreadable context fabricated a handoff: kind=%q, source=%q", database.handoffKind, database.completedSource)
			}
		})
	}
}

// The remaining Store methods deliberately have no implementation: unreadable
// or incompatible context cannot authorize retries or settlement of old evidence.
type contextErrorCorroborationStore struct {
	terminalCorroborationStore
	lease, resultLease       store.JobLease
	checkpoint               store.TerminalCorroboration
	contextErr               error
	handoffKind, handoffCode string
	handoffReason            workflow.Reason
	completedSource          string
}

func newContextErrorCorroborationStore(kind string, contextErr error) *contextErrorCorroborationStore {
	return &contextErrorCorroborationStore{
		lease: store.JobLease{Job: store.Job{
			JobSpec: store.JobSpec{Queue: "agent-turn-recovery", Kind: kind, WorkflowID: "workflow", WorkflowAttemptID: "attempt", AgentAssignmentID: "participant", AgentSessionID: "session", AgentTurnID: "source-turn", ExecutionEpoch: 7},
			ID:      "verification-job", Status: store.JobLeased, LeaseOwner: "verifier", LeaseToken: "exact-token", AttemptCount: 2,
		}, Attempt: 2},
		checkpoint: store.TerminalCorroboration{TurnID: "source-turn", ExecutionEpoch: 7, SourceInvocationID: "original-terminal-mutation", PromptOutcome: json.RawMessage(`{"stop_reason":"end_turn"}`)},
		contextErr: contextErr,
	}
}

func (d *contextErrorCorroborationStore) ClaimJobKind(_ context.Context, _, kind, _ string, _ time.Duration) (*store.JobLease, error) {
	if kind != d.lease.Kind {
		return nil, nil
	}
	return &d.lease, nil
}
func (*contextErrorCorroborationStore) HeartbeatJob(context.Context, store.JobLease, time.Duration) error {
	return nil
}
func (d *contextErrorCorroborationStore) GetTerminalCorroborationContext(context.Context, store.JobLease) (store.TerminalCorroborationContext, error) {
	// The real Store returns no usable context when prompt validation fails.
	return store.TerminalCorroborationContext{}, d.contextErr
}
func (d *contextErrorCorroborationStore) CompleteTerminalCorroborationHandoff(_ context.Context, lease store.JobLease, reason workflow.Reason, code string) (store.AgentTurnSettlement, error) {
	d.completeHandoff(lease, reason, code, store.VerifyTerminalIntentJobKind)
	return store.AgentTurnSettlement{}, nil
}
func (d *contextErrorCorroborationStore) CompleteTerminalRevalidationHandoff(_ context.Context, lease store.JobLease, reason workflow.Reason, code string) error {
	d.completeHandoff(lease, reason, code, store.RevalidateTerminalIntentJobKind)
	return nil
}
func (d *contextErrorCorroborationStore) completeHandoff(lease store.JobLease, reason workflow.Reason, code, kind string) {
	d.resultLease, d.handoffReason, d.handoffCode, d.handoffKind = lease, reason, code, kind
	if reflect.DeepEqual(lease, d.lease) && lease.AgentTurnID == d.checkpoint.TurnID && lease.ExecutionEpoch == d.checkpoint.ExecutionEpoch {
		d.completedSource = d.checkpoint.SourceInvocationID
	}
}
