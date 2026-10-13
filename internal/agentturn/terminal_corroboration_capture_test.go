package agentturn_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/agentturn"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

func TestDeferredCorroborationUsesRetainedSourceCaptureAfterRestartAndReplacement(t *testing.T) {
	for _, test := range []struct {
		name           string
		dirty, missing bool
	}{
		{name: "clean original"}, {name: "dirty original", dirty: true}, {name: "missing original", missing: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := outcomeRequest(t, workflow.RoleDeveloper, true)
			if test.dirty {
				request.Capture = outcomeCapture(t, true)
			}
			value, _ := outcomeCaptureOwners.Load(request.Capture.SHA256)
			owner := value.(*outcomeCaptureOwner)
			// Replacement belongs to a later execution. It cannot supply or hide
			// evidence from the pending original Turn.
			if err := os.RemoveAll(owner.paths.Workspace); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(owner.paths.Workspace, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(owner.paths.Workspace, "replacement.txt"), []byte("different execution"), 0o644); err != nil {
				t.Fatal(err)
			}
			// Publication caches are disposable and absent from the restore set.
			if err := os.RemoveAll(owner.paths.Publication); err != nil {
				t.Fatal(err)
			}
			if test.missing {
				if err := os.RemoveAll(owner.captureRoot); err != nil {
					t.Fatal(err)
				}
			}
			// A fresh service has no in-memory reference and no capture source.
			// Only the deterministic original Turn key and sealed manifest survive.
			restarted, err := workspace.New(workspace.Options{
				WorkspaceRoot:   filepath.Dir(filepath.Dir(owner.paths.Workspace)),
				PublicationRoot: filepath.Dir(filepath.Dir(owner.paths.Publication)),
				MiseRoot:        filepath.Join(filepath.Dir(owner.captureRoot), "mise"), CaptureRoot: owner.captureRoot,
			})
			if err != nil {
				t.Fatal(err)
			}
			owner.lifecycle = restarted
			resolver := &corroborationCaptureResolver{workspace: restarted}
			database := newCaptureCorroborationStore(request)
			reconciler := newOutcomeReconciler(t, &outcomeStore{}, &outcomeGitHub{pullRequest: outcomePullRequest(outcomeHead)})
			credentials := &executionCredentialProvider{operations: &executionOperations{}, credential: outcomeCredential}
			worker, err := agentturn.NewTerminalCorroborationWorker(database, reconciler, credentials, credentials, resolver,
				agentturn.TerminalCorroborationWorkerConfig{ClaimOwner: "verifier", Window: time.Hour, PollInterval: time.Millisecond, LeaseDuration: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			processed, err := worker.ProcessOne(context.Background())
			if !processed || err != nil {
				t.Fatalf("ProcessOne = %t, %v", processed, err)
			}
			if resolver.key != executionKeyForTest(request.Execution) {
				t.Fatalf("resolved wrong source: %#v", resolver.key)
			}
			switch {
			case test.missing:
				if database.retryCode != "workspace_observation_unavailable" || database.settled != nil || database.handoffCode != "" {
					t.Fatalf("missing source was accepted: %#v", database)
				}
			case test.dirty:
				if database.handoffCode != "terminal_evidence_conflict" || database.settled != nil || database.retryCode != "" {
					t.Fatalf("dirty source was accepted: %#v", database)
				}
			default:
				if database.settled == nil || database.settled.Outcome != workflow.TurnOutcomeChangeProposalReady || database.retryCode != "" || database.handoffCode != "" {
					t.Fatalf("retained clean source not accepted: %#v", database)
				}
			}
			if database.resultLease.ID != database.lease.ID || database.resultLease.LeaseToken != database.lease.LeaseToken || database.resultLease.Attempt != database.lease.Attempt {
				t.Fatalf("verification result lost job fence")
			}
		})
	}
}

func TestDeferredReviewerCorroborationDoesNotRequireTerminalCapture(t *testing.T) {
	request := outcomeRequest(t, workflow.RoleReviewer, true)
	database := newCaptureCorroborationStore(request)
	database.pending.Mutations = []store.MutationReservation{outcomeSubmitReviewMutation(1, "APPROVE", "APPROVED", outcomeHead, 701)}
	reconciler := newOutcomeReconciler(t, &outcomeStore{}, &outcomeGitHub{pullRequest: outcomePullRequest(outcomeHead), reviews: []githubapi.Review{outcomeReview(outcomeHead, 701)}})
	credentials := &executionCredentialProvider{operations: &executionOperations{}, credential: outcomeCredential}
	resolver := &executionWorkspace{operations: &executionOperations{}, err: errors.New("Reviewer capture discarded " + outcomeCredential)}
	worker, err := agentturn.NewTerminalCorroborationWorker(database, reconciler, credentials, credentials, resolver,
		agentturn.TerminalCorroborationWorkerConfig{ClaimOwner: "verifier", Window: time.Hour, PollInterval: time.Millisecond, LeaseDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	processed, err := worker.ProcessOne(context.Background())
	if !processed || err != nil || database.settled == nil || database.settled.Outcome != workflow.TurnOutcomeApproved || database.retryCode != "" || database.handoffCode != "" {
		t.Fatalf("Reviewer without terminal capture = processed %t, error %v, settlement %#v, retry %q, handoff %q", processed, err, database.settled, database.retryCode, database.handoffCode)
	}
}

type corroborationCaptureResolver struct {
	workspace agentturn.ExecutionWorkspace
	key       execution.Key
}

func (resolver *corroborationCaptureResolver) TerminalCapture(ctx context.Context, key execution.Key) (execution.CaptureRef, error) {
	resolver.key = key
	return resolver.workspace.TerminalCapture(ctx, key)
}

type captureCorroborationStore struct {
	lease, resultLease     store.JobLease
	pending                store.TerminalCorroborationContext
	settled                *store.AgentTurnSettlementObservation
	retryCode, handoffCode string
}

func newCaptureCorroborationStore(request agentturn.OutcomeReconciliation) *captureCorroborationStore {
	return &captureCorroborationStore{
		lease: store.JobLease{Job: store.Job{ID: outcomeJobID, LeaseToken: outcomeSessionID, JobSpec: store.JobSpec{Kind: store.VerifyTerminalIntentJobKind, WorkflowID: outcomeWorkflowID}}, Attempt: 2},
		pending: store.TerminalCorroborationContext{
			Execution: request.Execution, Mutations: []store.MutationReservation{outcomeRequestReviewMutation(1)},
			Checkpoint: store.TerminalCorroboration{WorkflowID: outcomeWorkflowID, TurnID: outcomeTurnID, ExecutionEpoch: 7,
				SourceInvocationID: "mutation-1", PendingSince: time.Now(), PromptOutcome: json.RawMessage(`{"admitted":true,"status":"completed"}`)},
		},
	}
}

func (database *captureCorroborationStore) ClaimJobKind(context.Context, string, string, string, time.Duration) (*store.JobLease, error) {
	return &database.lease, nil
}
func (*captureCorroborationStore) HeartbeatJob(context.Context, store.JobLease, time.Duration) error {
	return nil
}
func (database *captureCorroborationStore) GetTerminalCorroborationContext(context.Context, store.JobLease) (store.TerminalCorroborationContext, error) {
	return database.pending, nil
}
func (database *captureCorroborationStore) RetryTerminalCorroboration(_ context.Context, lease store.JobLease, code string, _ time.Duration) error {
	database.resultLease, database.retryCode = lease, code
	return nil
}
func (database *captureCorroborationStore) SettleTerminalCorroboration(_ context.Context, lease store.JobLease, observation store.AgentTurnSettlementObservation) (store.AgentTurnSettlement, error) {
	database.resultLease, database.settled = lease, &observation
	return store.AgentTurnSettlement{}, nil
}
func (database *captureCorroborationStore) CompleteTerminalCorroborationHandoff(_ context.Context, lease store.JobLease, _ workflow.Reason, code string) (store.AgentTurnSettlement, error) {
	database.resultLease, database.handoffCode = lease, code
	return store.AgentTurnSettlement{}, nil
}
func (*captureCorroborationStore) SettleTerminalRevalidation(context.Context, store.JobLease, int64, store.AgentTurnSettlementObservation) error {
	panic("unexpected revalidation")
}
func (*captureCorroborationStore) CompleteTerminalRevalidationHandoff(context.Context, store.JobLease, workflow.Reason, string) error {
	panic("unexpected revalidation handoff")
}

func (*captureCorroborationStore) RedirectRevalidationToFreshTurn(context.Context, store.JobLease, int64, string, int64, int64) error {
	panic("unexpected revalidation redirect")
}
