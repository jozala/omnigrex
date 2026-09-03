//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestCompleteWebhookTransitionCreatesFirstWorkflowWithoutConcreteAssignments(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	claim := claimWorkflowDelivery(t, databases[0], ctx, workflowDelivery("40000000-0000-4000-8000-000000000001"))
	observedAt := time.Date(2026, time.September, 2, 10, 0, 0, 0, time.UTC)
	var storedReceivedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT received_at FROM webhook_deliveries WHERE delivery_id = $1`, claim.DeliveryID).Scan(&storedReceivedAt); err != nil {
		t.Fatalf("query original received_at: %v", err)
	}
	if claim.ReceivedAt.IsZero() || !claim.ReceivedAt.Equal(storedReceivedAt) {
		t.Fatalf("claim ReceivedAt = %v, want original inbox timestamp %v", claim.ReceivedAt, storedReceivedAt)
	}

	application, err := databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "trigger"), workflowLocator(), func(snapshot workflow.Snapshot) workflow.Decision {
			return workflow.Reduce(snapshot, workflow.TriggerEvent{
				EventMetadata: workflow.EventMetadata{
					ID: claim.DeliveryID, ObservedAt: observedAt,
					WorkItem:         workflow.WorkItem{RepositoryID: 9123, IssueID: 456, IssueNumber: 12},
					ExpectedRevision: snapshot.Revision,
				},
				AttemptID: "50000000-0000-4000-8000-000000000001", AttemptNumber: snapshot.LastAttemptNumber + 1,
			})
		})
	if err != nil {
		t.Fatalf("CompleteWebhookTransition() error = %v", err)
	}
	if application.WorkflowID == "" || application.Status != store.NormalizedEventCompleted || application.Disposition != workflow.DispositionApplied || application.State != workflow.StateDeveloping || application.Revision != 1 {
		t.Fatalf("application = %#v, want applied DEVELOPING revision 1", application)
	}

	assertWorkflowApplicationRows(t, pool, claim.DeliveryID, application.WorkflowID)
}

func TestGetWorkflowRepositoryReturnsImmutableOwnerAndName(t *testing.T) {
	databases, _ := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	claim := claimWorkflowDelivery(t, databases[0], ctx, workflowDelivery("40000000-0000-4000-8000-000000000019"))
	application, err := databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "trigger"), workflowLocator(), triggerTransition(claim.DeliveryID, "50000000-0000-4000-8000-000000000019"))
	if err != nil {
		t.Fatalf("CompleteWebhookTransition() error = %v", err)
	}

	repository, err := databases[0].GetWorkflowRepository(ctx, application.WorkflowID)
	if err != nil {
		t.Fatalf("GetWorkflowRepository() error = %v", err)
	}
	if repository.Owner != "jozala" || repository.Name != "omnigrex" {
		t.Errorf("GetWorkflowRepository() = %#v, want jozala/omnigrex", repository)
	}
	if _, err := databases[0].GetWorkflowRepository(ctx, "not-a-workflow"); !errors.Is(err, store.ErrWorkflowNotFound) {
		t.Errorf("GetWorkflowRepository(invalid) error = %v, want ErrWorkflowNotFound", err)
	}
}

func TestCompleteWebhookTransitionRollsBackEveryWriteAndLeavesInboxReclaimable(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	delivery := workflowDelivery("40000000-0000-4000-8000-000000000002")
	if inserted, err := databases[0].InsertWebhookDelivery(ctx, delivery); err != nil || !inserted {
		t.Fatalf("InsertWebhookDelivery() = (%t, %v), want inserted", inserted, err)
	}
	claim, err := databases[0].ClaimWebhookDelivery(ctx, "failing-worker", 100*time.Millisecond)
	if err != nil || claim == nil {
		t.Fatalf("ClaimWebhookDelivery() = (%#v, %v), want claim", claim, err)
	}
	_, err = databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "trigger"), workflowLocator(), triggerTransition(claim.DeliveryID, "not-a-uuid"))
	if err == nil {
		t.Fatal("CompleteWebhookTransition() forced action failure error = nil")
	}
	var deliveryStatus string
	var workflows, events, jobs int
	if err := pool.QueryRow(ctx, `SELECT status FROM webhook_deliveries WHERE delivery_id = $1`, delivery.DeliveryID).Scan(&deliveryStatus); err != nil {
		t.Fatalf("query failed delivery: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM workflows), (SELECT count(*) FROM normalized_events), (SELECT count(*) FROM jobs)`).Scan(&workflows, &events, &jobs); err != nil {
		t.Fatalf("count rolled-back rows: %v", err)
	}
	if deliveryStatus != "PROCESSING" || workflows != 0 || events != 0 || jobs != 0 {
		t.Errorf("rollback state = status %s, workflows %d, events %d, jobs %d; want PROCESSING and zero writes", deliveryStatus, workflows, events, jobs)
	}
	time.Sleep(125 * time.Millisecond)
	reclaimed, err := databases[0].ClaimWebhookDelivery(ctx, "replacement-worker", time.Second)
	if err != nil || reclaimed == nil || reclaimed.DeliveryID != delivery.DeliveryID || reclaimed.AttemptCount != 2 {
		t.Fatalf("reclaimed delivery = (%#v, %v), want second-attempt claim", reclaimed, err)
	}
}

func TestCompleteWebhookTransitionRejectsMismatchedNormalizedDeliveryTransactionally(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	claim := claimWorkflowDelivery(t, databases[0], ctx, workflowDelivery("40000000-0000-4000-8000-000000000012"))
	called := false

	_, err := databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload("40000000-0000-4000-8000-000000000099", "trigger"), workflowLocator(), func(snapshot workflow.Snapshot) workflow.Decision {
			called = true
			return triggerTransition(claim.DeliveryID, "50000000-0000-4000-8000-000000000012")(snapshot)
		})
	if !errors.Is(err, store.ErrNormalizedEventDeliveryMismatch) {
		t.Fatalf("CompleteWebhookTransition() error = %v, want ErrNormalizedEventDeliveryMismatch", err)
	}
	if called {
		t.Error("transition called for mismatched normalized payload")
	}
	var status string
	var events, workflows int
	if err := pool.QueryRow(ctx, `SELECT status FROM webhook_deliveries WHERE delivery_id = $1`, claim.DeliveryID).Scan(&status); err != nil {
		t.Fatalf("query delivery status: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM normalized_events), (SELECT count(*) FROM workflows)`).Scan(&events, &workflows); err != nil {
		t.Fatalf("count rolled-back rows: %v", err)
	}
	if status != "PROCESSING" || events != 0 || workflows != 0 {
		t.Errorf("mismatch rollback = status %s, events %d, workflows %d; want PROCESSING, 0, 0", status, events, workflows)
	}
}

func TestConcurrentFirstTriggersCreateOnlyOneActiveAttempt(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	claims := make([]*store.WebhookClaim, 0, 2)
	for _, id := range []string{
		"40000000-0000-4000-8000-000000000003",
		"40000000-0000-4000-8000-000000000004",
	} {
		if inserted, err := databases[0].InsertWebhookDelivery(ctx, workflowDelivery(id)); err != nil || !inserted {
			t.Fatalf("insert concurrent delivery %s = (%t, %v)", id, inserted, err)
		}
	}
	for range 2 {
		claim, err := databases[0].ClaimWebhookDelivery(ctx, "concurrent-worker", time.Second)
		if err != nil || claim == nil {
			t.Fatalf("claim concurrent delivery = (%#v, %v)", claim, err)
		}
		claims = append(claims, claim)
	}

	start := make(chan struct{})
	applications := make(chan store.WorkflowApplication, 2)
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	for index, claim := range claims {
		wait.Add(1)
		go func(index int, claim *store.WebhookClaim) {
			defer wait.Done()
			<-start
			application, err := databases[index].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
				normalizedPayload(claim.DeliveryID, "trigger"), workflowLocator(),
				triggerTransition(claim.DeliveryID, []string{"50000000-0000-4000-8000-000000000003", "50000000-0000-4000-8000-000000000004"}[index]))
			applications <- application
			errs <- err
		}(index, claim)
	}
	close(start)
	wait.Wait()
	close(applications)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent CompleteWebhookTransition() error = %v", err)
		}
	}
	dispositions := make([]string, 0, 2)
	workflowIDs := map[string]bool{}
	for application := range applications {
		dispositions = append(dispositions, string(application.Disposition))
		workflowIDs[application.WorkflowID] = true
	}
	sort.Strings(dispositions)
	if len(workflowIDs) != 1 || len(dispositions) != 2 || dispositions[0] != "APPLIED" || dispositions[1] != "DUPLICATE" {
		t.Errorf("concurrent applications = workflows %#v, dispositions %#v; want one Workflow and APPLIED/DUPLICATE", workflowIDs, dispositions)
	}
	var activeAttempts, totalWorkflows int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM workflows), (SELECT count(*) FROM workflow_attempts WHERE active)`).Scan(&totalWorkflows, &activeAttempts); err != nil {
		t.Fatalf("count concurrent result: %v", err)
	}
	if totalWorkflows != 1 || activeAttempts != 1 {
		t.Errorf("concurrent result = %d workflows, %d active attempts; want 1 and 1", totalWorkflows, activeAttempts)
	}
}

func TestSameWorkflowTransitionsSerializeByRevision(t *testing.T) {
	databases, _ := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	initialClaim := claimWorkflowDelivery(t, databases[0], ctx, workflowDelivery("40000000-0000-4000-8000-000000000005"))
	if _, err := databases[0].CompleteWebhookTransition(ctx, initialClaim.DeliveryID, initialClaim.ClaimToken,
		normalizedPayload(initialClaim.DeliveryID, "trigger"), workflowLocator(), triggerTransition(initialClaim.DeliveryID, "50000000-0000-4000-8000-000000000005")); err != nil {
		t.Fatalf("initial CompleteWebhookTransition() error = %v", err)
	}

	claims := make([]*store.WebhookClaim, 0, 2)
	for _, id := range []string{"40000000-0000-4000-8000-000000000006", "40000000-0000-4000-8000-000000000007"} {
		if inserted, err := databases[0].InsertWebhookDelivery(ctx, workflowDelivery(id)); err != nil || !inserted {
			t.Fatalf("insert serial delivery %s = (%t, %v)", id, inserted, err)
		}
	}
	for range 2 {
		claim, err := databases[0].ClaimWebhookDelivery(ctx, "serial-worker", time.Second)
		if err != nil || claim == nil {
			t.Fatalf("claim serial delivery = (%#v, %v)", claim, err)
		}
		claims = append(claims, claim)
	}

	start := make(chan struct{})
	revisions := make(chan uint64, 2)
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	for index, claim := range claims {
		wait.Add(1)
		go func(index int, claim *store.WebhookClaim) {
			defer wait.Done()
			<-start
			application, err := databases[index].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
				normalizedPayload(claim.DeliveryID, "serial"), workflowLocator(), func(snapshot workflow.Snapshot) workflow.Decision {
					next := snapshot
					next.Revision++
					return workflow.Decision{Snapshot: next, Disposition: workflow.DispositionApplied, Reason: workflow.ReasonTriggered}
				})
			revisions <- application.Revision
			errs <- err
		}(index, claim)
	}
	close(start)
	wait.Wait()
	close(revisions)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("serialized CompleteWebhookTransition() error = %v", err)
		}
	}
	got := make([]int, 0, 2)
	for revision := range revisions {
		got = append(got, int(revision))
	}
	sort.Ints(got)
	if len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Errorf("serialized revisions = %v, want [2 3]", got)
	}
}

func TestCorroboratingEventDuringActiveTurnIsDeferredForThatTurn(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE workflows SET status = 'DEVELOPING', desired_assignment_status = 'ACTIVE', desired_runtime_state = 'ACTIVE' WHERE id = $1`, fixture.workflowID); err != nil {
		t.Fatalf("make Workflow reducer-compatible: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit = 1 WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatalf("set reducer attempt budget: %v", err)
	}
	turn, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatalf("AllocateAgentTurn() error = %v", err)
	}
	delivery := workflowDelivery("40000000-0000-4000-8000-000000000008")
	delivery.RepositoryID, delivery.IssueID, delivery.IssueNumber = 1, 1, 1
	delivery.RepositoryOwner, delivery.RepositoryName = "owner", "repo"
	claim := claimWorkflowDelivery(t, databases[0], ctx, delivery)
	locator := store.WorkflowLocator{RepositoryID: 1, IssueID: 1, IssueNumber: 1}
	application, err := databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "synchronize"), locator, func(snapshot workflow.Snapshot) workflow.Decision {
			return workflow.Reduce(snapshot, workflow.SynchronizationEvent{
				EventMetadata: workflow.EventMetadata{
					ID: claim.DeliveryID, ObservedAt: time.Now(), WorkItem: snapshot.WorkItem,
					ExpectedRevision: snapshot.Revision,
				},
				ChangeProposalID: 99, PreviousHeadSHA: "old", HeadSHA: "new",
			})
		})
	if err != nil {
		t.Fatalf("CompleteWebhookTransition() error = %v", err)
	}
	if application.Status != store.NormalizedEventDeferred || application.Disposition != workflow.DispositionDeferred || application.Revision != 1 {
		t.Errorf("deferred application = %#v, want DEFERRED at revision 1", application)
	}
	event, err := databases[0].GetNormalizedEvent(ctx, claim.DeliveryID)
	if err != nil {
		t.Fatalf("GetNormalizedEvent() error = %v", err)
	}
	if event.DeferredForTurnID != turn.ID || event.ProcessedAt == nil {
		t.Errorf("deferred event = %#v, want current turn %s and processed timestamp", event, turn.ID)
	}
}

func TestReviewObservationWithoutActiveTurnIsUnrelatedWithoutConsumingBudget(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 5)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE workflows SET status = 'REVIEWING', state_revision = 4 WHERE id = $1`, fixture.workflowID); err != nil {
		t.Fatalf("make reviewing Workflow: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET review_cycles_completed = 1, infrastructure_failure_limit = 1 WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatalf("set attempt budgets: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO change_proposals (id, workflow_id, repository_id, repository_owner, repository_name, pull_request_id, pull_request_number, status, base_ref, base_sha, head_ref, head_sha) VALUES ('60000000-0000-4000-8000-000000000005', $1, 5, 'owner', 'repo', 55, 15, 'OPEN', 'main', 'base', 'feature', 'head')`, fixture.workflowID); err != nil {
		t.Fatalf("seed reviewed Change Proposal: %v", err)
	}
	delivery := workflowDelivery("40000000-0000-4000-8000-000000000017")
	delivery.RepositoryID, delivery.RepositoryOwner, delivery.RepositoryName = 5, "owner", "repo"
	delivery.IssueID, delivery.IssueNumber = 0, 0
	claim := claimWorkflowDelivery(t, databases[0], ctx, delivery)

	application, err := databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "review"), store.WorkflowLocator{RepositoryID: 5, PullRequestID: 55}, func(snapshot workflow.Snapshot) workflow.Decision {
			return workflow.Reduce(snapshot, workflow.ReviewObservedEvent{
				EventMetadata: workflow.EventMetadata{ID: claim.DeliveryID, ObservedAt: claim.ReceivedAt, WorkItem: snapshot.WorkItem, ExpectedRevision: snapshot.Revision},
				Review:        workflow.ReviewIdentity{ID: 77, NodeID: "PRR_node", ChangeProposalID: 55, ActorID: 88, HeadSHA: "head"},
			})
		})
	if err != nil || application.Status != store.NormalizedEventCompleted || application.Disposition != workflow.DispositionUnrelated || application.Reason != workflow.ReasonCorroborationWithoutActiveTurn || application.Revision != 4 {
		t.Fatalf("review application = (%#v, %v), want unrelated completion at revision 4", application, err)
	}
	var used, reviews int
	if err := pool.QueryRow(ctx, `SELECT review_cycles_completed FROM workflow_attempts WHERE id = $1`, fixture.attemptID).Scan(&used); err != nil {
		t.Fatalf("query review budget: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM change_proposal_reviews`).Scan(&reviews); err != nil {
		t.Fatalf("count persisted reviews: %v", err)
	}
	if used != 1 || reviews != 0 {
		t.Errorf("review side effects = budget %d, persisted reviews %d; want 1 and 0", used, reviews)
	}
}

func TestSynchronizationGuardsPreviousHeadAndUpdatesExistingChangeProposal(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 7)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE workflows SET status = 'REVIEWING' WHERE id = $1`, fixture.workflowID); err != nil {
		t.Fatalf("make reviewing Workflow: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit = 1 WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatalf("set attempt budget: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO change_proposals (id, workflow_id, repository_id, repository_owner, repository_name, pull_request_id, pull_request_number, status, base_ref, base_sha, head_ref, head_sha) VALUES ('60000000-0000-4000-8000-000000000007', $1, 7, 'owner', 'repo', 77, 17, 'OPEN', 'main', 'base', 'feature', 'old-head')`, fixture.workflowID); err != nil {
		t.Fatalf("seed synchronized Change Proposal: %v", err)
	}

	apply := func(deliveryID, previousHead string) store.WorkflowApplication {
		delivery := workflowDelivery(deliveryID)
		delivery.RepositoryID, delivery.RepositoryOwner, delivery.RepositoryName = 7, "owner", "repo"
		delivery.IssueID, delivery.IssueNumber = 0, 0
		claim := claimWorkflowDelivery(t, databases[0], ctx, delivery)
		application, err := databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
			normalizedPayload(claim.DeliveryID, "synchronize"), store.WorkflowLocator{RepositoryID: 7, PullRequestID: 77}, func(snapshot workflow.Snapshot) workflow.Decision {
				return workflow.Reduce(snapshot, workflow.SynchronizationEvent{
					EventMetadata:    workflow.EventMetadata{ID: claim.DeliveryID, ObservedAt: claim.ReceivedAt, WorkItem: snapshot.WorkItem, ExpectedRevision: snapshot.Revision},
					ChangeProposalID: 77, PreviousHeadSHA: previousHead, HeadSHA: "new-head",
				})
			})
		if err != nil {
			t.Fatalf("CompleteWebhookTransition(%s) error = %v", previousHead, err)
		}
		return application
	}

	stale := apply("40000000-0000-4000-8000-000000000019", "wrong-head")
	if stale.Disposition != workflow.DispositionStale || stale.Reason != workflow.ReasonSynchronizationStale || stale.Revision != 1 {
		t.Fatalf("stale synchronization = %#v, want STALE revision 1", stale)
	}
	var head string
	if err := pool.QueryRow(ctx, `SELECT head_sha FROM change_proposals WHERE pull_request_id = 77`).Scan(&head); err != nil {
		t.Fatalf("query stale head: %v", err)
	}
	if head != "old-head" {
		t.Fatalf("head after stale synchronization = %q, want old-head", head)
	}

	applied := apply("40000000-0000-4000-8000-000000000020", "old-head")
	if applied.Disposition != workflow.DispositionApplied || applied.Revision != 2 {
		t.Fatalf("matching synchronization = %#v, want APPLIED revision 2", applied)
	}
	var proposals int
	if err := pool.QueryRow(ctx, `SELECT count(*), min(head_sha) FROM change_proposals WHERE workflow_id = $1`, fixture.workflowID).Scan(&proposals, &head); err != nil {
		t.Fatalf("query updated Change Proposal: %v", err)
	}
	if proposals != 1 || head != "new-head" {
		t.Errorf("Change Proposals after synchronization = %d rows at %q, want one at new-head", proposals, head)
	}
}

func TestAppliedTransitionCannotManufactureChangeProposal(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 6)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE workflows SET status = 'DEVELOPING' WHERE id = $1`, fixture.workflowID); err != nil {
		t.Fatalf("make developing Workflow: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit = 1 WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatalf("set attempt budget: %v", err)
	}
	delivery := workflowDelivery("40000000-0000-4000-8000-000000000018")
	delivery.RepositoryID, delivery.RepositoryOwner, delivery.RepositoryName = 6, "owner", "repo"
	delivery.IssueID, delivery.IssueNumber = 6, 6
	claim := claimWorkflowDelivery(t, databases[0], ctx, delivery)

	_, err := databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "proposal"), store.WorkflowLocator{RepositoryID: 6, IssueID: 6, IssueNumber: 6}, func(snapshot workflow.Snapshot) workflow.Decision {
			next := snapshot
			next.State = workflow.StateReviewing
			next.Revision++
			next.ChangeProposal = &workflow.ChangeProposal{ID: 66, Number: 16, HeadSHA: "head", Open: true}
			return workflow.Decision{Snapshot: next, Disposition: workflow.DispositionApplied, Reason: workflow.ReasonChangeProposalReady}
		})
	if err == nil {
		t.Fatal("CompleteWebhookTransition() error = nil, want missing relational Change Proposal error")
	}
	var proposals int
	var state string
	var revision int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM change_proposals`).Scan(&proposals); err != nil {
		t.Fatalf("count Change Proposals: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status, state_revision FROM workflows WHERE id = $1`, fixture.workflowID).Scan(&state, &revision); err != nil {
		t.Fatalf("query rolled-back Workflow: %v", err)
	}
	if proposals != 0 || state != "DEVELOPING" || revision != 1 {
		t.Errorf("rolled-back proposal transition = proposals %d, state %s, revision %d; want 0, DEVELOPING, 1", proposals, state, revision)
	}
}

func TestApplyNextPendingNormalizedEventAppliesHistoricalEventOnce(t *testing.T) {
	databases, _ := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	claim := claimWorkflowDelivery(t, databases[0], ctx, workflowDelivery("40000000-0000-4000-8000-000000000009"))
	payload := normalizedPayload(claim.DeliveryID, "trigger")
	if err := databases[0].CompleteWebhookDelivery(ctx, claim.DeliveryID, claim.ClaimToken, store.WebhookCompletion{
		Outcome: store.WebhookOutcomeProcessed, NormalizedPayload: normalizedPayload("40000000-0000-4000-8000-000000000099", "trigger"),
	}); !errors.Is(err, store.ErrNormalizedEventDeliveryMismatch) {
		t.Fatalf("CompleteWebhookDelivery() mismatch error = %v, want ErrNormalizedEventDeliveryMismatch", err)
	}
	if err := databases[0].CompleteWebhookDelivery(ctx, claim.DeliveryID, claim.ClaimToken, store.WebhookCompletion{Outcome: store.WebhookOutcomeProcessed, NormalizedPayload: payload}); err != nil {
		t.Fatalf("CompleteWebhookDelivery() compatibility write error = %v", err)
	}
	factoryCalls := 0
	application, applied, err := databases[0].ApplyNextPendingNormalizedEvent(ctx, func(record store.NormalizedEventRecord) (store.WorkflowLocator, store.WorkflowTransition, error) {
		factoryCalls++
		var decoded map[string]string
		decodeErr := json.Unmarshal(record.Payload, &decoded)
		if record.Status != store.NormalizedEventPending || decodeErr != nil || decoded["kind"] != "trigger" {
			t.Errorf("pending record = %#v, want original PENDING payload", record)
		}
		return workflowLocator(), triggerTransition(record.DeliveryID, "50000000-0000-4000-8000-000000000009"), nil
	})
	if err != nil || !applied || application.Disposition != workflow.DispositionApplied {
		t.Fatalf("ApplyNextPendingNormalizedEvent() = (%#v, %t, %v), want applied", application, applied, err)
	}
	if _, applied, err := databases[0].ApplyNextPendingNormalizedEvent(ctx, func(store.NormalizedEventRecord) (store.WorkflowLocator, store.WorkflowTransition, error) {
		return store.WorkflowLocator{}, nil, errors.New("must not be called")
	}); err != nil || applied {
		t.Errorf("second ApplyNextPendingNormalizedEvent() = (%t, %v), want (false, nil)", applied, err)
	}
	if factoryCalls != 1 {
		t.Errorf("factory calls = %d, want 1", factoryCalls)
	}
}

func TestApplyNextPendingNormalizedEventRejectsMismatchedDeliveryAndLeavesItPending(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	delivery := workflowDelivery("40000000-0000-4000-8000-000000000013")
	if inserted, err := databases[0].InsertWebhookDelivery(ctx, delivery); err != nil || !inserted {
		t.Fatalf("InsertWebhookDelivery() = (%t, %v), want inserted", inserted, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE webhook_deliveries SET status = 'PROCESSED', processed_at = clock_timestamp() WHERE delivery_id = $1`, delivery.DeliveryID); err != nil {
		t.Fatalf("seed processed delivery: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO normalized_events (delivery_id, payload) VALUES ($1, $2)`, delivery.DeliveryID,
		normalizedPayload("40000000-0000-4000-8000-000000000099", "trigger")); err != nil {
		t.Fatalf("seed mismatched pending event: %v", err)
	}
	called := false

	_, applied, err := databases[0].ApplyNextPendingNormalizedEvent(ctx, func(store.NormalizedEventRecord) (store.WorkflowLocator, store.WorkflowTransition, error) {
		called = true
		return workflowLocator(), triggerTransition(delivery.DeliveryID, "50000000-0000-4000-8000-000000000013"), nil
	})
	if applied || !errors.Is(err, store.ErrNormalizedEventDeliveryMismatch) {
		t.Fatalf("ApplyNextPendingNormalizedEvent() = (%t, %v), want mismatch without application", applied, err)
	}
	if called {
		t.Error("factory called for mismatched historical payload")
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM normalized_events WHERE delivery_id = $1`, delivery.DeliveryID).Scan(&status); err != nil {
		t.Fatalf("query pending event: %v", err)
	}
	if status != "PENDING" {
		t.Errorf("normalized event status = %s, want PENDING after rollback", status)
	}
}

func TestWorkflowMarkerAssociatesPullRequestAndConflictsWithRelation(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	first := seedAgentSession(t, pool, 3)
	second := seedAgentSession(t, pool, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, fixture := range []agentFixture{first, second} {
		if _, err := pool.Exec(ctx, `UPDATE workflows SET status = 'DEVELOPING' WHERE id = $1`, fixture.workflowID); err != nil {
			t.Fatalf("make Workflow reducer-compatible: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit = 1 WHERE id = $1`, fixture.attemptID); err != nil {
			t.Fatalf("set attempt budget: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE workflows SET repository_id = 3 WHERE id = $1`, second.workflowID); err != nil {
		t.Fatalf("place marker conflict Workflow in repository: %v", err)
	}

	markerDelivery := workflowDelivery("40000000-0000-4000-8000-000000000014")
	markerDelivery.RepositoryID, markerDelivery.RepositoryOwner, markerDelivery.RepositoryName = 3, "owner", "repo"
	markerDelivery.IssueID, markerDelivery.IssueNumber = 0, 0
	claim := claimWorkflowDelivery(t, databases[0], ctx, markerDelivery)
	application, err := databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "opened"), store.WorkflowLocator{RepositoryID: 3, PullRequestID: 99, WorkflowID: first.workflowID}, func(snapshot workflow.Snapshot) workflow.Decision {
			return workflow.Reduce(snapshot, workflow.ChangeProposalObservedEvent{
				EventMetadata:  workflow.EventMetadata{ID: claim.DeliveryID, ObservedAt: claim.ReceivedAt, WorkItem: snapshot.WorkItem, ExpectedRevision: snapshot.Revision},
				ChangeProposal: workflow.ChangeProposal{ID: 99, Number: 9, HeadSHA: "head", Open: true},
			})
		})
	if err != nil || application.WorkflowID != first.workflowID || application.Disposition != workflow.DispositionUnrelated {
		t.Fatalf("marker application = (%#v, %v), want first Workflow unrelated without an active turn", application, err)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO change_proposals (id, workflow_id, repository_id, repository_owner, repository_name, pull_request_id, pull_request_number, status, base_ref, base_sha, head_ref, head_sha) VALUES ('60000000-0000-4000-8000-000000000001', $1, 3, 'owner', 'repo', 100, 10, 'OPEN', 'main', 'base', 'feature', 'head')`, first.workflowID); err != nil {
		t.Fatalf("seed Change Proposal relation: %v", err)
	}
	conflictDelivery := markerDelivery
	conflictDelivery.DeliveryID = "40000000-0000-4000-8000-000000000015"
	claim = claimWorkflowDelivery(t, databases[0], ctx, conflictDelivery)
	_, err = databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "opened"), store.WorkflowLocator{RepositoryID: 3, PullRequestID: 100, WorkflowID: second.workflowID}, func(snapshot workflow.Snapshot) workflow.Decision {
			return workflow.Decision{Snapshot: snapshot, Disposition: workflow.DispositionUnrelated, Reason: workflow.ReasonChangeProposalUnrelated}
		})
	if !errors.Is(err, store.ErrWorkflowLocatorMismatch) {
		t.Fatalf("relationship/marker conflict error = %v, want ErrWorkflowLocatorMismatch", err)
	}
}

func TestMissingWorkflowMarkerDoesNotCreateWorkflowFromPullRequest(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	delivery := workflowDelivery("40000000-0000-4000-8000-000000000016")
	delivery.IssueID, delivery.IssueNumber = 0, 0
	claim := claimWorkflowDelivery(t, databases[0], ctx, delivery)
	application, err := databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "opened"), store.WorkflowLocator{RepositoryID: 9123, PullRequestID: 99, WorkflowID: "70000000-0000-4000-8000-000000000001"}, func(snapshot workflow.Snapshot) workflow.Decision {
			return workflow.Reduce(snapshot, workflow.ChangeProposalObservedEvent{
				EventMetadata:  workflow.EventMetadata{ID: claim.DeliveryID, ObservedAt: claim.ReceivedAt, WorkItem: workflow.WorkItem{RepositoryID: 9123, IssueID: 99, IssueNumber: 9}, ExpectedRevision: snapshot.Revision},
				ChangeProposal: workflow.ChangeProposal{ID: 99, Number: 9, HeadSHA: "head", Open: true},
			})
		})
	if err != nil || application.WorkflowID != "" || application.Disposition != workflow.DispositionUnrelated {
		t.Fatalf("missing marker application = (%#v, %v), want unrelated without Workflow", application, err)
	}
	var workflows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflows`).Scan(&workflows); err != nil {
		t.Fatalf("count Workflows: %v", err)
	}
	if workflows != 0 {
		t.Errorf("Workflow count = %d, want 0", workflows)
	}
}

func TestIssueClosureClosesMutationAdmissionBeforeEnqueuingStop(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE workflows SET status = 'DEVELOPING', desired_assignment_status = 'ACTIVE', desired_runtime_state = 'ACTIVE' WHERE id = $1`, fixture.workflowID); err != nil {
		t.Fatalf("make Workflow reducer-compatible: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit = 1 WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatalf("set reducer attempt budget: %v", err)
	}
	turn, err := databases[0].AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatalf("AllocateAgentTurn() error = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_turns SET mutation_admission_open = TRUE, status = 'RUNNING' WHERE id = $1`, turn.ID); err != nil {
		t.Fatalf("open mutation admission fixture: %v", err)
	}
	delivery := workflowDelivery("40000000-0000-4000-8000-000000000010")
	delivery.RepositoryID, delivery.IssueID, delivery.IssueNumber = 2, 2, 2
	delivery.RepositoryOwner, delivery.RepositoryName = "owner", "repo"
	claim := claimWorkflowDelivery(t, databases[0], ctx, delivery)
	observedAt := time.Now().UTC()
	application, err := databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "closed"), store.WorkflowLocator{RepositoryID: 2, IssueID: 2, IssueNumber: 2}, func(snapshot workflow.Snapshot) workflow.Decision {
			return workflow.Reduce(snapshot, workflow.IssueClosedEvent{
				EventMetadata: workflow.EventMetadata{ID: claim.DeliveryID, ObservedAt: observedAt, WorkItem: snapshot.WorkItem, ExpectedRevision: snapshot.Revision},
				ClosureID:     "closure-2", RetainUntil: observedAt.Add(24 * time.Hour), RetentionToken: "retention-2",
			})
		})
	if err != nil {
		t.Fatalf("CompleteWebhookTransition() close error = %v", err)
	}
	if application.State != workflow.StateClosing {
		t.Errorf("closure state = %s, want CLOSING", application.State)
	}
	var admissionOpen bool
	var turnStatus string
	var stopJobs int
	if err := pool.QueryRow(ctx, `SELECT mutation_admission_open, status FROM agent_turns WHERE id = $1`, turn.ID).Scan(&admissionOpen, &turnStatus); err != nil {
		t.Fatalf("query closed turn: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE normalized_event_id = $1 AND kind = 'STOP_AGENT_TURN'`, claim.DeliveryID).Scan(&stopJobs); err != nil {
		t.Fatalf("count stop jobs: %v", err)
	}
	if admissionOpen || turnStatus != "CANCELLING" || stopJobs != 1 {
		t.Errorf("closed turn = admission %t, status %s, stop jobs %d; want false, CANCELLING, 1", admissionOpen, turnStatus, stopJobs)
	}
}

func triggerTransition(deliveryID, attemptID string) store.WorkflowTransition {
	return func(snapshot workflow.Snapshot) workflow.Decision {
		return workflow.Reduce(snapshot, workflow.TriggerEvent{
			EventMetadata: workflow.EventMetadata{
				ID: deliveryID, ObservedAt: time.Date(2026, time.September, 2, 10, 0, 0, 0, time.UTC),
				WorkItem: workflow.WorkItem{RepositoryID: 9123, IssueID: 456, IssueNumber: 12}, ExpectedRevision: snapshot.Revision,
			},
			AttemptID: attemptID, AttemptNumber: snapshot.LastAttemptNumber + 1,
		})
	}
}

func workflowDelivery(deliveryID string) store.WebhookDelivery {
	return store.WebhookDelivery{
		DeliveryID: deliveryID, EventName: "issues", Action: "labeled",
		RepositoryID: 9123, RepositoryOwner: "jozala", RepositoryName: "omnigrex",
		IssueID: 456, IssueNumber: 12, Headers: map[string]string{}, Payload: []byte(`{}`),
	}
}

func workflowLocator() store.WorkflowLocator {
	return store.WorkflowLocator{RepositoryID: 9123, IssueID: 456, IssueNumber: 12}
}

func normalizedPayload(deliveryID, kind string) json.RawMessage {
	payload, _ := json.Marshal(map[string]string{"delivery_id": deliveryID, "kind": kind})
	return payload
}

func claimWorkflowDelivery(t *testing.T, database *store.Store, ctx context.Context, delivery store.WebhookDelivery) *store.WebhookClaim {
	t.Helper()
	if inserted, err := database.InsertWebhookDelivery(ctx, delivery); err != nil || !inserted {
		t.Fatalf("InsertWebhookDelivery() = (%t, %v), want (true, nil)", inserted, err)
	}
	claim, err := database.ClaimWebhookDelivery(ctx, "workflow-test", 30*time.Second)
	if err != nil || claim == nil {
		t.Fatalf("ClaimWebhookDelivery() = (%#v, %v), want claim", claim, err)
	}
	return claim
}

func assertWorkflowApplicationRows(t *testing.T, pool *pgxpool.Pool, deliveryID, workflowID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var state string
	var revision int64
	var assignmentStatus, runtimeState string
	if err := pool.QueryRow(ctx, `SELECT status, state_revision, desired_assignment_status, desired_runtime_state FROM workflows WHERE id = $1`, workflowID).Scan(&state, &revision, &assignmentStatus, &runtimeState); err != nil {
		t.Fatalf("query workflow: %v", err)
	}
	if state != "DEVELOPING" || revision != 1 || assignmentStatus != "ACTIVE" || runtimeState != "ACTIVE" {
		t.Errorf("workflow state = (%s, %d, %s, %s), want (DEVELOPING, 1, ACTIVE, ACTIVE)", state, revision, assignmentStatus, runtimeState)
	}
	var attempts, assignments int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_attempts WHERE workflow_id = $1 AND active`, workflowID).Scan(&attempts); err != nil {
		t.Fatalf("count active attempts: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_assignments WHERE workflow_id = $1`, workflowID).Scan(&assignments); err != nil {
		t.Fatalf("count assignments: %v", err)
	}
	if attempts != 1 || assignments != 0 {
		t.Errorf("durable hierarchy = %d active attempts, %d assignments, want 1 and 0", attempts, assignments)
	}
	var total, prepare, labels int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE kind = 'PREPARE_AGENT_TURN'), count(*) FILTER (WHERE kind = 'RECONCILE_GITHUB_LABELS') FROM jobs WHERE normalized_event_id = $1`, deliveryID).Scan(&total, &prepare, &labels); err != nil {
		t.Fatalf("count action jobs: %v", err)
	}
	if total != 2 || prepare != 1 || labels != 1 {
		t.Errorf("action jobs = %d total, %d prepare, %d labels, want exactly two with one each", total, prepare, labels)
	}
	event, err := pool.Query(ctx, `SELECT event.status, event.disposition, event.reason, event.applied_revision, event.workflow_id::text, delivery.status, delivery.claim_token IS NULL FROM normalized_events event JOIN webhook_deliveries delivery USING (delivery_id) WHERE event.delivery_id = $1`, deliveryID)
	if err != nil {
		t.Fatalf("query event completion: %v", err)
	}
	defer event.Close()
	if !event.Next() {
		t.Fatal("normalized completion row missing")
	}
	var eventStatus, disposition, reason, eventWorkflowID, deliveryStatus string
	var eventRevision int64
	var unclaimed bool
	if err := event.Scan(&eventStatus, &disposition, &reason, &eventRevision, &eventWorkflowID, &deliveryStatus, &unclaimed); err != nil {
		t.Fatalf("scan event completion: %v", err)
	}
	if eventStatus != "COMPLETED" || disposition != "APPLIED" || reason != "workflow_triggered" || eventRevision != 1 || eventWorkflowID != workflowID || deliveryStatus != "PROCESSED" || !unclaimed {
		t.Errorf("event completion = (%s, %s, %s, %d, %s, %s, %t), want completed application", eventStatus, disposition, reason, eventRevision, eventWorkflowID, deliveryStatus, unclaimed)
	}
}
