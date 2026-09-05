package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

const (
	testWorkflowID = "workflow-1"
	testJobID      = "job-1"
	testCredential = "developer-installation-secret"
	testReadySHA   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testNewSHA     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestManagedLabelStateMapsWorkflowStates(t *testing.T) {
	tests := []struct {
		state workflow.State
		want  WorkflowState
	}{
		{workflow.StateDeveloping, StateDeveloping},
		{workflow.StateReviewing, StateReviewing},
		{workflow.StatePRReady, StatePRReady},
		{workflow.StateNeedsHuman, StateNeedsHuman},
		{workflow.StateDormant, StateNone},
		{workflow.StateClosing, StateNone},
		{workflow.StateClosed, StateNone},
	}
	for _, test := range tests {
		got, err := managedLabelState(test.state)
		if err != nil || got != test.want {
			t.Errorf("managedLabelState(%s) = (%q, %v), want %q", test.state, got, err, test.want)
		}
	}
	if _, err := managedLabelState(workflow.StateAbsent); !errors.Is(err, ErrInvalidWorkflowState) {
		t.Errorf("managedLabelState(ABSENT) error = %v, want ErrInvalidWorkflowState", err)
	}
}

func TestLabelWorkerClaimsExactKindAndReconcilesLatestStateOnIssueAndPullRequest(t *testing.T) {
	workerStore := newFakeVisibleEffectStore(store.ReconcileGitHubLabelsJobKind, testEffect(workflow.StateReviewing, 3))
	workerStore.effect.JobRevision = 2
	api := newFakeVisibleEffectAPI()
	api.issueLabels[17] = []Label{{Name: string(StateRun)}, {Name: string(StateDeveloping)}, {Name: "priority:high"}}
	api.issueLabels[23] = []Label{{Name: string(StatePRReady)}, {Name: "security-review"}}
	worker := newTestLabelWorker(t, workerStore, api, testWorkerConfig())

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want processed", processed, err)
	}
	if workerStore.claimedKind != store.ReconcileGitHubLabelsJobKind {
		t.Errorf("claim kind = %s, want exact label kind", workerStore.claimedKind)
	}
	assertLabelNames(t, api.issueLabels[17], "priority:high", string(StateReviewing))
	assertLabelNames(t, api.issueLabels[23], "security-review", string(StateReviewing))
	if workerStore.ackRevision != 3 || workerStore.ackState != workflow.StateReviewing || workerStore.failure != nil {
		t.Errorf("acknowledgement = revision %d state %s failure %v", workerStore.ackRevision, workerStore.ackState, workerStore.failure)
	}
	if api.getPullRequestCalls != 0 {
		t.Errorf("GetPullRequest calls = %d, want none outside PR_READY", api.getPullRequestCalls)
	}
}

func TestLabelWorkerPRReadyHeadRaceClearsManagedStateAndRetries(t *testing.T) {
	effect := testEffect(workflow.StatePRReady, 4)
	effect.ReadyForSHA = testReadySHA
	effect.ChangeProposal.HeadSHA = testReadySHA
	workerStore := newFakeVisibleEffectStore(store.ReconcileGitHubLabelsJobKind, effect)
	api := newFakeVisibleEffectAPI()
	api.pullRequest.Head.SHA = testNewSHA
	api.issueLabels[17] = []Label{{Name: string(StatePRReady)}, {Name: "human:issue"}}
	api.issueLabels[23] = []Label{{Name: string(StateReviewing)}, {Name: string(StatePRReady)}, {Name: "human:pr"}}
	worker := newTestLabelWorker(t, workerStore, api, testWorkerConfig())

	processed, err := worker.ProcessNext(context.Background())
	if !processed || err == nil || !strings.Contains(err.Error(), ErrStalePullRequestHead.Error()) {
		t.Fatalf("ProcessNext() = (%t, %v), want retryable stale-head failure", processed, err)
	}
	assertLabelNames(t, api.issueLabels[17], "human:issue")
	assertLabelNames(t, api.issueLabels[23], "human:pr")
	if api.getPullRequestCalls != 1 || workerStore.failure == nil || !workerStore.failureRetryable || workerStore.acknowledged {
		t.Errorf("head-race handling = GetPR %d failure %v retryable %t acknowledged %t", api.getPullRequestCalls, workerStore.failure, workerStore.failureRetryable, workerStore.acknowledged)
	}
}

func TestLabelWorkerPRReadyRequiresRemoteAndBothDurableHeads(t *testing.T) {
	tests := []struct {
		name        string
		ready       string
		durableHead string
		remoteHead  string
		wantReady   bool
	}{
		{"all match", testReadySHA, testReadySHA, testReadySHA, true},
		{"durable proposal differs", testReadySHA, testNewSHA, testReadySHA, false},
		{"ready SHA missing", "", testReadySHA, testReadySHA, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			effect := testEffect(workflow.StatePRReady, 4)
			effect.ReadyForSHA = test.ready
			effect.ChangeProposal.HeadSHA = test.durableHead
			workerStore := newFakeVisibleEffectStore(store.ReconcileGitHubLabelsJobKind, effect)
			api := newFakeVisibleEffectAPI()
			api.pullRequest.Head.SHA = test.remoteHead
			worker := newTestLabelWorker(t, workerStore, api, testWorkerConfig())
			_, err := worker.ProcessNext(context.Background())
			if test.wantReady {
				if err != nil {
					t.Fatalf("ProcessNext() error = %v", err)
				}
				assertLabelNames(t, api.issueLabels[17], string(StatePRReady))
				assertLabelNames(t, api.issueLabels[23], string(StatePRReady))
			} else if err == nil {
				t.Fatal("ProcessNext() error = nil, want stale-head failure")
			}
			if api.getPullRequestCalls != 1 {
				t.Errorf("GetPullRequest calls = %d, want 1", api.getPullRequestCalls)
			}
		})
	}
}

func TestHumanHandoffWorkerPublishesIssueAndPullRequestIdempotently(t *testing.T) {
	effect := testEffect(workflow.StateNeedsHuman, 5)
	effect.HandoffReason = string(workflow.ReasonReviewBudgetExhausted)
	effect.HandoffDiagnostic = "Reviewer found a blocking compatibility problem."
	api := newFakeVisibleEffectAPI()
	firstStore := newFakeVisibleEffectStore(store.PublishHumanHandoffJobKind, effect)
	worker := newTestHandoffWorker(t, firstStore, api, testWorkerConfig())

	if processed, err := worker.ProcessNext(context.Background()); err != nil || !processed {
		t.Fatalf("first ProcessNext() = (%t, %v)", processed, err)
	}
	if firstStore.claimedKind != store.PublishHumanHandoffJobKind || api.issueCreates != 1 || api.pullRequestCreates != 1 {
		t.Fatalf("publication = claim %s Issue creates %d PR creates %d", firstStore.claimedKind, api.issueCreates, api.pullRequestCreates)
	}
	assertCredentialFreeCommentResult(t, firstStore.ackResult)

	secondStore := newFakeVisibleEffectStore(store.PublishHumanHandoffJobKind, effect)
	worker = newTestHandoffWorker(t, secondStore, api, testWorkerConfig())
	if _, err := worker.ProcessNext(context.Background()); err != nil {
		t.Fatalf("idempotent ProcessNext() error = %v", err)
	}
	if api.issueCreates != 1 || api.pullRequestCreates != 1 {
		t.Errorf("idempotent retry created comments: Issue %d PR %d", api.issueCreates, api.pullRequestCreates)
	}
}

func TestHumanHandoffWorkerRecoversAmbiguousCreateByMarker(t *testing.T) {
	effect := testEffect(workflow.StateNeedsHuman, 5)
	effect.HandoffReason = string(workflow.ReasonAgentBlocked)
	workerStore := newFakeVisibleEffectStore(store.PublishHumanHandoffJobKind, effect)
	api := newFakeVisibleEffectAPI()
	api.ambiguousCreate[17] = true
	api.createErrors[17] = &TransientError{Cause: errors.New("connection reset")}
	worker := newTestHandoffWorker(t, workerStore, api, testWorkerConfig())

	if _, err := worker.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext() ambiguous create error = %v", err)
	}
	if api.issueCreates != 1 || len(api.comments[17]) != 1 || workerStore.failure != nil {
		t.Errorf("ambiguous recovery = creates %d comments %d failure %v", api.issueCreates, len(api.comments[17]), workerStore.failure)
	}
}

func TestHumanHandoffWorkerConvergesDuplicateMarkersAndPreservesOtherComments(t *testing.T) {
	effect := testEffect(workflow.StateNeedsHuman, 5)
	effect.HandoffReason = string(workflow.ReasonAgentBlocked)
	workerStore := newFakeVisibleEffectStore(store.PublishHumanHandoffJobKind, effect)
	api := newFakeVisibleEffectAPI()
	marker, err := RenderMarker(Marker{WorkflowID: testWorkflowID, OperationID: testJobID})
	if err != nil {
		t.Fatal(err)
	}
	otherMarker, err := RenderMarker(Marker{WorkflowID: testWorkflowID, OperationID: "other-job"})
	if err != nil {
		t.Fatal(err)
	}
	api.comments[17] = []IssueComment{
		{ID: 2, Body: "second\n\n" + marker},
		{ID: 1, Body: "first\n\n" + marker},
		{ID: 3, Body: "other operation\n\n" + otherMarker},
		{ID: 4, Body: "human comment"},
	}
	worker := newTestHandoffWorker(t, workerStore, api, testWorkerConfig())

	if _, processErr := worker.ProcessNext(context.Background()); processErr != nil {
		t.Fatalf("ProcessNext() error = %v", processErr)
	}
	marked := exactMarkedComments(api.comments[17], Marker{WorkflowID: testWorkflowID, OperationID: testJobID})
	if len(marked) != 1 || marked[0].ID != 1 || fmt.Sprint(api.deletedCommentIDs) != "[2]" || api.issueCreates != 0 {
		t.Errorf("convergence = marked %#v deleted %v creates %d", marked, api.deletedCommentIDs, api.issueCreates)
	}
	if len(api.comments[17]) != 3 || api.comments[17][1].ID != 3 || api.comments[17][2].ID != 4 {
		t.Errorf("unrelated comments changed: %#v", api.comments[17])
	}
}

func TestHumanHandoffWorkerConvergesLateDuplicateObservedOnRetry(t *testing.T) {
	effect := testEffect(workflow.StateNeedsHuman, 5)
	effect.HandoffReason = string(workflow.ReasonAgentBlocked)
	workerStore := newFakeVisibleEffectStore(store.PublishHumanHandoffJobKind, effect)
	api := newFakeVisibleEffectAPI()
	marker, err := RenderMarker(Marker{WorkflowID: testWorkflowID, OperationID: testJobID})
	if err != nil {
		t.Fatal(err)
	}
	api.comments[17] = []IssueComment{{ID: 101, Body: "retry\n\n" + marker}, {ID: 102, Body: "late completion\n\n" + marker}}
	worker := newTestHandoffWorker(t, workerStore, api, testWorkerConfig())

	if _, err := worker.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}
	if marked := exactMarkedComments(api.comments[17], Marker{WorkflowID: testWorkflowID, OperationID: testJobID}); len(marked) != 1 || marked[0].ID != 101 {
		t.Errorf("late duplicate did not converge: %#v", marked)
	}
	if api.issueCreates != 0 {
		t.Errorf("retry created another Issue comment: %d", api.issueCreates)
	}
}

func TestHumanHandoffWorkerHandlesAmbiguousAndNotFoundDeletesIdempotently(t *testing.T) {
	for _, test := range []struct {
		name      string
		deleteErr error
	}{
		{name: "ambiguous transport failure", deleteErr: &TransientError{Cause: errors.New("connection reset")}},
		{name: "already deleted", deleteErr: &APIError{StatusCode: 404, Method: "DELETE", Path: "/issues/comments/2", Message: "not found"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			effect := testEffect(workflow.StateNeedsHuman, 5)
			effect.HandoffReason = string(workflow.ReasonAgentBlocked)
			workerStore := newFakeVisibleEffectStore(store.PublishHumanHandoffJobKind, effect)
			api := newFakeVisibleEffectAPI()
			marker, err := RenderMarker(Marker{WorkflowID: testWorkflowID, OperationID: testJobID})
			if err != nil {
				t.Fatal(err)
			}
			api.comments[17] = []IssueComment{{ID: 1, Body: "first\n\n" + marker}, {ID: 2, Body: "second\n\n" + marker}}
			api.deleteErrors[2] = test.deleteErr
			api.ambiguousDelete[2] = true
			worker := newTestHandoffWorker(t, workerStore, api, testWorkerConfig())

			if _, err := worker.ProcessNext(context.Background()); err != nil {
				t.Fatalf("ProcessNext() error = %v", err)
			}
			if len(exactMarkedComments(api.comments[17], Marker{WorkflowID: testWorkflowID, OperationID: testJobID})) != 1 || workerStore.failure != nil {
				t.Errorf("delete did not converge: comments %#v failure %v", api.comments[17], workerStore.failure)
			}
		})
	}
}

func TestHumanHandoffWorkerRemovesCommentsWhenAcknowledgementIsSuperseded(t *testing.T) {
	effect := testEffect(workflow.StateNeedsHuman, 5)
	effect.HandoffReason = string(workflow.ReasonAgentBlocked)
	workerStore := newFakeVisibleEffectStore(store.PublishHumanHandoffJobKind, effect)
	workerStore.ackAcknowledgement.Superseded = true
	workerStore.ackAcknowledgement.CleanupRequired = true
	api := newFakeVisibleEffectAPI()
	worker := newTestHandoffWorker(t, workerStore, api, testWorkerConfig())

	if _, err := worker.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}
	if !workerStore.acknowledged || !workerStore.cleanupAcknowledged || len(api.comments[17]) != 0 || len(api.comments[23]) != 0 || len(api.deletedCommentIDs) != 2 {
		t.Errorf("superseded cleanup = acknowledged %t cleanup acknowledged %t Issue %#v PR %#v deletes %v", workerStore.acknowledged, workerStore.cleanupAcknowledged, api.comments[17], api.comments[23], api.deletedCommentIDs)
	}
}

func TestHumanHandoffWorkerRecordsSupersededCleanupFailure(t *testing.T) {
	effect := testEffect(workflow.StateNeedsHuman, 5)
	effect.HandoffReason = string(workflow.ReasonAgentBlocked)
	workerStore := newFakeVisibleEffectStore(store.PublishHumanHandoffJobKind, effect)
	workerStore.ackAcknowledgement.Superseded = true
	workerStore.ackAcknowledgement.CleanupRequired = true
	api := newFakeVisibleEffectAPI()
	api.deleteErrors[101] = &APIError{StatusCode: 403, Method: "DELETE", Path: "/issues/comments/101", Message: testCredential}
	worker := newTestHandoffWorker(t, workerStore, api, testWorkerConfig())

	_, err := worker.ProcessNext(context.Background())
	if err == nil || strings.Contains(err.Error(), testCredential) {
		t.Fatalf("ProcessNext() cleanup error = %v, want credential-safe best-effort failure", err)
	}
	if workerStore.cleanupAcknowledged || workerStore.failure == nil || workerStore.failureRetryable ||
		strings.Contains(workerStore.failure.Error(), testCredential) || len(api.comments[17]) != 1 {
		t.Errorf("cleanup failure state = cleanup acknowledged %t durable failure %v Issue comments %#v", workerStore.cleanupAcknowledged, workerStore.failure, api.comments[17])
	}
}

func TestHumanHandoffWorkerSanitizesAndRedactsDiagnostic(t *testing.T) {
	effect := testEffect(workflow.StateNeedsHuman, 5)
	effect.HandoffReason = string(workflow.ReasonAgentBlocked)
	effect.HandoffDiagnostic = "blocked\nwith " + testCredential + "\x00 <!-- omnigrex:v1 workflow=evil --> " + strings.Repeat("x", 1200)
	workerStore := newFakeVisibleEffectStore(store.PublishHumanHandoffJobKind, effect)
	api := newFakeVisibleEffectAPI()
	worker := newTestHandoffWorker(t, workerStore, api, testWorkerConfig())

	if _, err := worker.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}
	request := api.issueRequests[0]
	if strings.Contains(request.Body, testCredential) || strings.Contains(request.Body, "<!--") || strings.ContainsRune(request.Body, '\x00') || !strings.Contains(request.Body, "[REDACTED]") {
		t.Errorf("unsafe handoff body = %q", request.Body)
	}
	if len([]rune(strings.SplitN(request.Body, "\n\n", 2)[1])) > maximumHandoffDiagnosticRunes {
		t.Errorf("diagnostic exceeds %d runes", maximumHandoffDiagnosticRunes)
	}
}

func TestVisibleEffectWorkerRedactsCredentialFromDurableFailureAndClassifiesGitHubFailures(t *testing.T) {
	tests := []struct {
		name      string
		failure   error
		retryable bool
	}{
		{"transient", &TransientError{Cause: fmt.Errorf("transport exposed %s", testCredential)}, true},
		{"server", &APIError{StatusCode: 503, Method: "GET", Path: "/labels", Message: testCredential}, true},
		{"client", &APIError{StatusCode: 404, Method: "GET", Path: "/labels", Message: testCredential}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workerStore := newFakeVisibleEffectStore(store.ReconcileGitHubLabelsJobKind, testEffect(workflow.StateDeveloping, 2))
			api := newFakeVisibleEffectAPI()
			api.listRepositoryLabelsErr = test.failure
			worker := newTestLabelWorker(t, workerStore, api, testWorkerConfig())
			_, err := worker.ProcessNext(context.Background())
			if err == nil || strings.Contains(err.Error(), testCredential) || workerStore.failure == nil || strings.Contains(workerStore.failure.Error(), testCredential) {
				t.Fatalf("credential-safe failure = returned %v durable %v", err, workerStore.failure)
			}
			if workerStore.failureRetryable != test.retryable {
				t.Errorf("retryable = %t, want %t", workerStore.failureRetryable, test.retryable)
			}
		})
	}
}

func TestVisibleEffectWorkerHeartbeatsDuringGitHubCall(t *testing.T) {
	workerStore := newFakeVisibleEffectStore(store.ReconcileGitHubLabelsJobKind, testEffect(workflow.StateDeveloping, 2))
	api := newFakeVisibleEffectAPI()
	api.blockRepositoryLabels = make(chan struct{})
	config := testWorkerConfig()
	config.LeaseDuration = 100 * time.Millisecond
	config.HeartbeatInterval = 5 * time.Millisecond
	worker := newTestLabelWorker(t, workerStore, api, config)
	done := make(chan error, 1)
	go func() {
		_, err := worker.ProcessNext(context.Background())
		done <- err
	}()

	select {
	case <-workerStore.heartbeatObserved:
	case <-time.After(time.Second):
		t.Fatal("worker did not heartbeat during blocked GitHub call")
	}
	close(api.blockRepositoryLabels)
	if err := <-done; err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}
	if workerStore.heartbeatExtension != config.LeaseDuration {
		t.Errorf("heartbeat extension = %s, want %s", workerStore.heartbeatExtension, config.LeaseDuration)
	}
}

func TestVisibleEffectWorkerStopsRemoteWorkWhenHeartbeatLosesFence(t *testing.T) {
	workerStore := newFakeVisibleEffectStore(store.ReconcileGitHubLabelsJobKind, testEffect(workflow.StateDeveloping, 2))
	workerStore.heartbeatErr = store.ErrJobLeaseLost
	api := newFakeVisibleEffectAPI()
	api.waitForContextOnRepositoryLabels = true
	config := testWorkerConfig()
	config.LeaseDuration = 100 * time.Millisecond
	config.HeartbeatInterval = 5 * time.Millisecond
	worker := newTestLabelWorker(t, workerStore, api, config)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || err == nil || !strings.Contains(err.Error(), store.ErrJobLeaseLost.Error()) {
		t.Fatalf("ProcessNext() = (%t, %v), want heartbeat fence loss", processed, err)
	}
	if workerStore.failure != nil || workerStore.acknowledged {
		t.Errorf("heartbeat fence loss durably settled stale lease: failure %v acknowledged %t", workerStore.failure, workerStore.acknowledged)
	}
}

func TestVisibleEffectFailureCompletesAsSupersededWhenStateChanges(t *testing.T) {
	workerStore := newFakeVisibleEffectStore(store.ReconcileGitHubLabelsJobKind, testEffect(workflow.StateDeveloping, 2))
	workerStore.failureAcknowledgement.Superseded = true
	api := newFakeVisibleEffectAPI()
	api.listRepositoryLabelsErr = &TransientError{Cause: errors.New("temporarily unavailable")}
	worker := newTestLabelWorker(t, workerStore, api, testWorkerConfig())

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed || workerStore.failure == nil {
		t.Fatalf("ProcessNext() = (%t, %v), durable failure %v; want superseded success", processed, err, workerStore.failure)
	}
}

func TestHumanHandoffStaleJobCompletesWithoutPublishing(t *testing.T) {
	effect := testEffect(workflow.StateReviewing, 8)
	effect.JobRevision = 7
	workerStore := newFakeVisibleEffectStore(store.PublishHumanHandoffJobKind, effect)
	workerStore.ackAcknowledgement.Superseded = true
	workerStore.ackAcknowledgement.CleanupRequired = true
	api := newFakeVisibleEffectAPI()
	worker := newTestHandoffWorker(t, workerStore, api, testWorkerConfig())

	if _, err := worker.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}
	if api.issueCreates != 0 || api.pullRequestCreates != 0 || !workerStore.cleanupAcknowledged || !strings.Contains(string(workerStore.ackResult), `"superseded":true`) {
		t.Errorf("stale handoff = Issue creates %d PR creates %d result %s", api.issueCreates, api.pullRequestCreates, workerStore.ackResult)
	}
}

func TestHumanHandoffWorkerRetriesCleanupAfterPostAcknowledgementCrash(t *testing.T) {
	effect := testEffect(workflow.StateNeedsHuman, 5)
	effect.HandoffReason = string(workflow.ReasonAgentBlocked)
	api := newFakeVisibleEffectAPI()
	firstStore := newFakeVisibleEffectStore(store.PublishHumanHandoffJobKind, effect)
	firstStore.ackErr = errors.New("database connection lost after post")
	firstWorker := newTestHandoffWorker(t, firstStore, api, testWorkerConfig())
	if _, err := firstWorker.ProcessNext(context.Background()); err == nil {
		t.Fatal("first ProcessNext() error = nil, want acknowledgement failure")
	}
	if len(api.comments[17]) != 1 || len(api.comments[23]) != 1 || firstStore.acknowledged {
		t.Fatalf("post/ack crash state = Issue %#v PR %#v acknowledged %t", api.comments[17], api.comments[23], firstStore.acknowledged)
	}

	retryEffect := testEffect(workflow.StateReviewing, 6)
	retryEffect.JobRevision = 5
	retryStore := newFakeVisibleEffectStore(store.PublishHumanHandoffJobKind, retryEffect)
	retryStore.ackAcknowledgement = store.WorkflowGitHubEffectAcknowledgement{Superseded: true, CleanupRequired: true}
	retryWorker := newTestHandoffWorker(t, retryStore, api, testWorkerConfig())
	if _, err := retryWorker.ProcessNext(context.Background()); err != nil {
		t.Fatalf("cleanup retry ProcessNext() error = %v", err)
	}
	if len(api.comments[17]) != 0 || len(api.comments[23]) != 0 || !retryStore.cleanupAcknowledged {
		t.Errorf("cleanup retry = Issue %#v PR %#v acknowledged %t", api.comments[17], api.comments[23], retryStore.cleanupAcknowledged)
	}
}

func TestHumanHandoffWorkerRetriesAfterCleanupBeforeCleanupAcknowledgementCrash(t *testing.T) {
	effect := testEffect(workflow.StateNeedsHuman, 5)
	effect.HandoffReason = string(workflow.ReasonAgentBlocked)
	api := newFakeVisibleEffectAPI()
	firstStore := newFakeVisibleEffectStore(store.PublishHumanHandoffJobKind, effect)
	firstStore.ackAcknowledgement = store.WorkflowGitHubEffectAcknowledgement{Superseded: true, CleanupRequired: true}
	firstStore.cleanupAckErr = errors.New("cleanup acknowledgement response lost")
	if _, err := newTestHandoffWorker(t, firstStore, api, testWorkerConfig()).ProcessNext(context.Background()); err == nil {
		t.Fatal("first ProcessNext() error = nil, want cleanup acknowledgement failure")
	}
	if len(api.comments[17]) != 0 || len(api.comments[23]) != 0 || firstStore.acknowledged {
		t.Fatalf("cleanup/ack crash state = Issue %#v PR %#v acknowledged %t", api.comments[17], api.comments[23], firstStore.acknowledged)
	}

	retryEffect := effect
	retryEffect.CleanupRequired = true
	retryEffect.CleanupIssueNumber = 17
	retryEffect.CleanupPullRequestNumber = 23
	retryStore := newFakeVisibleEffectStore(store.PublishHumanHandoffJobKind, retryEffect)
	if _, err := newTestHandoffWorker(t, retryStore, api, testWorkerConfig()).ProcessNext(context.Background()); err != nil {
		t.Fatalf("cleanup acknowledgement retry error = %v", err)
	}
	if api.issueCreates != 1 || api.pullRequestCreates != 1 || !retryStore.cleanupAcknowledged {
		t.Errorf("cleanup acknowledgement retry created comments or did not ack: Issue %d PR %d ack %t", api.issueCreates, api.pullRequestCreates, retryStore.cleanupAcknowledged)
	}
}

func TestHumanHandoffWorkerRetriesMarkerScopedCleanupAfterIssueBoundaryCrash(t *testing.T) {
	effect := testEffect(workflow.StateNeedsHuman, 5)
	effect.HandoffReason = string(workflow.ReasonAgentBlocked)
	api := newFakeVisibleEffectAPI()
	marker, err := RenderMarker(Marker{WorkflowID: testWorkflowID, OperationID: testJobID})
	if err != nil {
		t.Fatal(err)
	}
	otherMarker, err := RenderMarker(Marker{WorkflowID: testWorkflowID, OperationID: "other-job"})
	if err != nil {
		t.Fatal(err)
	}
	api.comments[17] = []IssueComment{{ID: 101, Body: "Issue\n\n" + marker}, {ID: 201, Body: "other\n\n" + otherMarker}}
	api.comments[23] = []IssueComment{{ID: 102, Body: "PR\n\n" + marker}, {ID: 202, Body: "human"}}
	api.listCommentErrors[23] = errors.New("worker crashed before PR cleanup")
	effect.CleanupRequired = true
	effect.CleanupIssueNumber = 17
	effect.CleanupPullRequestNumber = 23
	firstStore := newFakeVisibleEffectStore(store.PublishHumanHandoffJobKind, effect)
	if _, err := newTestHandoffWorker(t, firstStore, api, testWorkerConfig()).ProcessNext(context.Background()); err == nil {
		t.Fatal("first cleanup error = nil, want PR boundary failure")
	}
	if len(exactMarkedComments(api.comments[17], Marker{WorkflowID: testWorkflowID, OperationID: testJobID})) != 0 ||
		len(exactMarkedComments(api.comments[23], Marker{WorkflowID: testWorkflowID, OperationID: testJobID})) != 1 || firstStore.cleanupAcknowledged {
		t.Fatalf("partial cleanup = Issue %#v PR %#v ack %t", api.comments[17], api.comments[23], firstStore.cleanupAcknowledged)
	}
	delete(api.listCommentErrors, 23)
	retryStore := newFakeVisibleEffectStore(store.PublishHumanHandoffJobKind, effect)
	if _, err := newTestHandoffWorker(t, retryStore, api, testWorkerConfig()).ProcessNext(context.Background()); err != nil {
		t.Fatalf("retry cleanup error = %v", err)
	}
	if len(api.comments[17]) != 1 || api.comments[17][0].ID != 201 || len(api.comments[23]) != 1 || api.comments[23][0].ID != 202 {
		t.Errorf("marker-scoped retry changed unrelated comments: Issue %#v PR %#v", api.comments[17], api.comments[23])
	}
}

func newTestLabelWorker(t *testing.T, workerStore *fakeVisibleEffectStore, api *fakeVisibleEffectAPI, config VisibleEffectWorkerConfig) *LabelWorker {
	t.Helper()
	worker, err := NewLabelWorker(workerStore, fakeVisibleEffectCredentials{}, api, config)
	if err != nil {
		t.Fatalf("NewLabelWorker() error = %v", err)
	}
	return worker
}

func newTestHandoffWorker(t *testing.T, workerStore *fakeVisibleEffectStore, api *fakeVisibleEffectAPI, config VisibleEffectWorkerConfig) *HumanHandoffWorker {
	t.Helper()
	worker, err := NewHumanHandoffWorker(workerStore, fakeVisibleEffectCredentials{}, api, config)
	if err != nil {
		t.Fatalf("NewHumanHandoffWorker() error = %v", err)
	}
	return worker
}

func testWorkerConfig() VisibleEffectWorkerConfig {
	return VisibleEffectWorkerConfig{
		ClaimOwner: "visible-effect-worker", LeaseDuration: time.Hour,
		HeartbeatInterval: time.Minute, IdlePollInterval: time.Millisecond, RetryDelay: 25 * time.Millisecond,
	}
}

func testEffect(state workflow.State, revision uint64) store.WorkflowGitHubEffectContext {
	return store.WorkflowGitHubEffectContext{
		WorkflowID: testWorkflowID, JobRevision: revision, RepositoryID: 99,
		RepositoryOwner: "acme", RepositoryName: "widgets", IssueNumber: 17,
		State: state, Revision: revision,
		ChangeProposal: &store.WorkflowGitHubChangeProposal{ID: 200, Number: 23, HeadSHA: testReadySHA, ReadyForSHA: testReadySHA},
	}
}

func assertLabelNames(t *testing.T, labels []Label, want ...string) {
	t.Helper()
	got := make([]string, len(labels))
	for index, label := range labels {
		got[index] = label.Name
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
}

func assertCredentialFreeCommentResult(t *testing.T, result json.RawMessage) {
	t.Helper()
	if strings.Contains(string(result), testCredential) || strings.Contains(string(result), "diagnostic") || strings.Contains(string(result), "body") {
		t.Fatalf("comment result contains non-reference data: %s", result)
	}
	var decoded handoffPublicationResult
	if err := json.Unmarshal(result, &decoded); err != nil || decoded.Issue.ID == 0 || decoded.Issue.URL == "" || decoded.PullRequest == nil || decoded.PullRequest.ID == 0 || decoded.PullRequest.URL == "" {
		t.Fatalf("comment result = %s, decode error %v", result, err)
	}
}

type fakeVisibleEffectCredentials struct{}

func (fakeVisibleEffectCredentials) RepositoryCredential(context.Context, string, string) (string, error) {
	return testCredential, nil
}

type fakeVisibleEffectStore struct {
	lease                  store.JobLease
	effect                 store.WorkflowGitHubEffectContext
	claimedQueue           string
	claimedKind            string
	acknowledged           bool
	ackRevision            uint64
	ackState               workflow.State
	ackResult              json.RawMessage
	failure                error
	failureRetryable       bool
	failureDelay           time.Duration
	failureAcknowledgement store.WorkflowGitHubEffectAcknowledgement
	ackAcknowledgement     store.WorkflowGitHubEffectAcknowledgement
	ackErr                 error
	cleanupAckErr          error
	cleanupAcknowledged    bool
	heartbeats             int
	heartbeatExtension     time.Duration
	heartbeatErr           error
	heartbeatObserved      chan struct{}
	heartbeatOnce          sync.Once
}

func newFakeVisibleEffectStore(kind string, effect store.WorkflowGitHubEffectContext) *fakeVisibleEffectStore {
	return &fakeVisibleEffectStore{
		lease:  store.JobLease{Job: store.Job{ID: testJobID, JobSpec: store.JobSpec{Queue: store.WorkflowActionQueue, Kind: kind, WorkflowID: testWorkflowID}}},
		effect: effect, heartbeatObserved: make(chan struct{}),
	}
}

func (fake *fakeVisibleEffectStore) ClaimWorkflowGitHubEffectJob(_ context.Context, kind, _ string, _ time.Duration) (*store.JobLease, error) {
	fake.claimedKind = kind
	return &fake.lease, nil
}

func (fake *fakeVisibleEffectStore) HeartbeatJob(_ context.Context, _ store.JobLease, extension time.Duration) error {
	fake.heartbeats++
	fake.heartbeatExtension = extension
	fake.heartbeatOnce.Do(func() { close(fake.heartbeatObserved) })
	return fake.heartbeatErr
}

func (fake *fakeVisibleEffectStore) GetWorkflowGitHubEffectContext(context.Context, store.JobLease) (store.WorkflowGitHubEffectContext, error) {
	return fake.effect, nil
}

func (fake *fakeVisibleEffectStore) AcknowledgeWorkflowGitHubEffect(_ context.Context, _ store.JobLease, effect store.WorkflowGitHubEffectContext, result json.RawMessage) (store.WorkflowGitHubEffectAcknowledgement, error) {
	fake.acknowledged = fake.ackErr == nil && !fake.ackAcknowledgement.CleanupRequired
	fake.ackRevision, fake.ackState = effect.Revision, effect.State
	fake.ackResult = append(json.RawMessage(nil), result...)
	return fake.ackAcknowledgement, fake.ackErr
}

func (fake *fakeVisibleEffectStore) AcknowledgeWorkflowGitHubEffectFailure(_ context.Context, _ store.JobLease, effect store.WorkflowGitHubEffectContext, cause error, retryable bool, delay time.Duration) (store.WorkflowGitHubEffectAcknowledgement, error) {
	fake.ackRevision, fake.ackState = effect.Revision, effect.State
	fake.failure, fake.failureRetryable, fake.failureDelay = cause, retryable, delay
	return fake.failureAcknowledgement, nil
}

func (fake *fakeVisibleEffectStore) AcknowledgeWorkflowGitHubEffectCleanup(context.Context, store.JobLease) (store.WorkflowGitHubEffectAcknowledgement, error) {
	if fake.cleanupAckErr != nil {
		return store.WorkflowGitHubEffectAcknowledgement{}, fake.cleanupAckErr
	}
	fake.cleanupAcknowledged = true
	fake.acknowledged = true
	return store.WorkflowGitHubEffectAcknowledgement{Superseded: true}, nil
}

type fakeVisibleEffectAPI struct {
	repositoryLabels                 []Label
	issueLabels                      map[int][]Label
	pullRequest                      PullRequest
	getPullRequestCalls              int
	listRepositoryLabelsErr          error
	blockRepositoryLabels            chan struct{}
	waitForContextOnRepositoryLabels bool
	comments                         map[int][]IssueComment
	listCommentErrors                map[int]error
	createErrors                     map[int]error
	ambiguousCreate                  map[int]bool
	issueRequests                    []CommentRequest
	pullRequestRequests              []CommentRequest
	issueCreates                     int
	pullRequestCreates               int
	nextCommentID                    int64
	deletedCommentIDs                []int64
	deleteErrors                     map[int64]error
	ambiguousDelete                  map[int64]bool
}

func newFakeVisibleEffectAPI() *fakeVisibleEffectAPI {
	return &fakeVisibleEffectAPI{
		repositoryLabels: ManagedLabels(), issueLabels: make(map[int][]Label), comments: make(map[int][]IssueComment),
		createErrors: make(map[int]error), ambiguousCreate: make(map[int]bool), nextCommentID: 100,
		listCommentErrors: make(map[int]error),
		deleteErrors:      make(map[int64]error), ambiguousDelete: make(map[int64]bool),
		pullRequest: PullRequest{Number: 23, Head: PullRequestBranch{SHA: testReadySHA}},
	}
}

func (fake *fakeVisibleEffectAPI) ListRepositoryLabels(ctx context.Context, _ string, _ string, _ string) ([]Label, error) {
	if fake.blockRepositoryLabels != nil {
		<-fake.blockRepositoryLabels
	}
	if fake.waitForContextOnRepositoryLabels {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return append([]Label(nil), fake.repositoryLabels...), fake.listRepositoryLabelsErr
}

func (fake *fakeVisibleEffectAPI) CreateRepositoryLabel(_ context.Context, _, _, _ string, label Label) (Label, error) {
	fake.repositoryLabels = append(fake.repositoryLabels, label)
	return label, nil
}

func (fake *fakeVisibleEffectAPI) ListIssueLabels(_ context.Context, _, _, _ string, number int) ([]Label, error) {
	return append([]Label(nil), fake.issueLabels[number]...), nil
}

func (fake *fakeVisibleEffectAPI) ReplaceIssueLabels(_ context.Context, _, _, _ string, number int, names []string) ([]Label, error) {
	fake.issueLabels[number] = labelsFromNames(names)
	return append([]Label(nil), fake.issueLabels[number]...), nil
}

func (fake *fakeVisibleEffectAPI) AddIssueLabels(_ context.Context, _, _, _ string, number int, names []string) ([]Label, error) {
	for _, name := range names {
		if !fakeHasLabel(fake.issueLabels[number], name) {
			fake.issueLabels[number] = append(fake.issueLabels[number], Label{Name: name})
		}
	}
	return append([]Label(nil), fake.issueLabels[number]...), nil
}

func (fake *fakeVisibleEffectAPI) RemoveIssueLabel(_ context.Context, _, _, _ string, number int, name string) error {
	labels := fake.issueLabels[number]
	for index, label := range labels {
		if label.Name == name {
			fake.issueLabels[number] = append(labels[:index], labels[index+1:]...)
			break
		}
	}
	return nil
}

func (fake *fakeVisibleEffectAPI) GetPullRequest(context.Context, string, string, string, int) (PullRequest, error) {
	fake.getPullRequestCalls++
	return fake.pullRequest, nil
}

func (fake *fakeVisibleEffectAPI) ListIssueComments(_ context.Context, _, _, _ string, number int) ([]IssueComment, error) {
	return append([]IssueComment(nil), fake.comments[number]...), fake.listCommentErrors[number]
}

func (fake *fakeVisibleEffectAPI) CreateIssueComment(_ context.Context, _, _, _ string, number int, request CommentRequest) (IssueComment, error) {
	fake.issueCreates++
	fake.issueRequests = append(fake.issueRequests, request)
	return fake.createComment(number, request)
}

func (fake *fakeVisibleEffectAPI) CreatePullRequestComment(_ context.Context, _, _, _ string, number int, request CommentRequest) (IssueComment, error) {
	fake.pullRequestCreates++
	fake.pullRequestRequests = append(fake.pullRequestRequests, request)
	return fake.createComment(number, request)
}

func (fake *fakeVisibleEffectAPI) DeleteIssueComment(_ context.Context, _, _, _ string, commentID int64) error {
	fake.deletedCommentIDs = append(fake.deletedCommentIDs, commentID)
	err := fake.deleteErrors[commentID]
	if err == nil || fake.ambiguousDelete[commentID] {
		for number, comments := range fake.comments {
			for index, comment := range comments {
				if comment.ID == commentID {
					fake.comments[number] = append(comments[:index], comments[index+1:]...)
					return err
				}
			}
		}
	}
	return err
}

func (fake *fakeVisibleEffectAPI) createComment(number int, request CommentRequest) (IssueComment, error) {
	fake.nextCommentID++
	comment := IssueComment{ID: fake.nextCommentID, Body: request.Body + "\n\n" + request.Marker, HTMLURL: fmt.Sprintf("https://github.com/acme/widgets/issues/%d#issuecomment-%d", number, fake.nextCommentID)}
	err := fake.createErrors[number]
	if err == nil || fake.ambiguousCreate[number] {
		fake.comments[number] = append(fake.comments[number], comment)
	}
	return comment, err
}

func labelsFromNames(names []string) []Label {
	labels := make([]Label, len(names))
	for index, name := range names {
		labels[index] = Label{Name: name}
	}
	return labels
}

func fakeHasLabel(labels []Label, name string) bool {
	for _, label := range labels {
		if label.Name == name {
			return true
		}
	}
	return false
}
