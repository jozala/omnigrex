//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func seedNeedsHumanWithProposal(t *testing.T, pool *pgxpool.Pool, workflowID, attemptID string, repositoryID int64, issueID, issueNumber int64, prID, prNumber int64, headSHA string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
UPDATE workflows
SET status = 'NEEDS_HUMAN', state_revision = 2,
    resume_role = 'DEVELOPER',
    desired_assignment_status = 'WAITING_FOR_HUMAN', desired_runtime_state = 'ACTIVE',
    human_handoff_reason = 'agent_blocked'
WHERE id = $1`, workflowID); err != nil {
		t.Fatalf("make NEEDS_HUMAN workflow: %v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE workflow_attempts
SET infrastructure_failure_limit = 1, current_stage = 'implementation', review_usage = '{}'::jsonb
WHERE id = $1`, attemptID); err != nil {
		t.Fatalf("make NEEDS_HUMAN attempt: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO change_proposals (id, workflow_id, repository_id, repository_owner, repository_name, pull_request_id, pull_request_number, status, base_ref, base_sha, head_ref, head_sha)
VALUES (gen_random_uuid(), $1, $2, 'owner', 'repo', $3, $4, 'OPEN', 'main', 'base', 'feature', $5)`,
		workflowID, repositoryID, prID, prNumber, headSHA); err != nil {
		t.Fatalf("seed current Change Proposal: %v", err)
	}
}

func jsonMarshalPRActivation(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

// TestPRActivationMatchesIssueReactivationFromNeedsHuman verifies the shared
// trigger path produces equivalent Workflow outcomes for Issue and eligible PR
// commands.
func TestPRActivationMatchesIssueReactivationFromNeedsHuman(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	makeWorkflow := func(number int, prID, prNumber int64) (string, string) {
		fixture := seedAgentSession(t, pool, number)
		seedNeedsHumanWithProposal(t, pool, fixture.workflowID, fixture.attemptID, int64(number), int64(number), int64(number), prID, prNumber, "durable-head")
		return fixture.workflowID, fixture.attemptID
	}

	issueWorkflowID, _ := makeWorkflow(910, 91000, 910)
	prWorkflowID, _ := makeWorkflow(911, 91100, 911)

	issueDelivery := workflowDelivery("40000000-0000-4000-8000-000000000910")
	issueDelivery.RepositoryID, issueDelivery.RepositoryOwner, issueDelivery.RepositoryName = 910, "owner", "repo"
	issueDelivery.IssueID, issueDelivery.IssueNumber = 910, 910
	issueClaim := claimWorkflowDelivery(t, databases[0], ctx, issueDelivery)
	issueApp, err := databases[0].CompleteWebhookTransition(ctx, issueClaim.DeliveryID, issueClaim.ClaimToken,
		normalizedPayload(issueClaim.DeliveryID, "trigger"),
		store.WorkflowLocator{RepositoryID: 910, IssueID: 910, IssueNumber: 910},
		triggerEventFactory("50000000-0000-4000-8000-000000000910"))
	if err != nil {
		t.Fatalf("Issue reactivation error = %v", err)
	}

	prDelivery := workflowDelivery("40000000-0000-4000-8000-000000000911")
	prDelivery.RepositoryID, prDelivery.RepositoryOwner, prDelivery.RepositoryName = 911, "owner", "repo"
	prDelivery.EventName, prDelivery.Action = "pull_request", "labeled"
	prDelivery.IssueID, prDelivery.IssueNumber = 0, 0
	prClaim := claimWorkflowDelivery(t, databases[0], ctx, prDelivery)
	prApp, err := databases[0].CompleteWebhookTransition(ctx, prClaim.DeliveryID, prClaim.ClaimToken,
		normalizedPayload(prClaim.DeliveryID, "trigger"),
		store.WorkflowLocator{RepositoryID: 911, PullRequestID: 91100, PullRequestNumber: 911, PullRequestActivation: true},
		triggerEventFactory("50000000-0000-4000-8000-000000000911"))
	if err != nil {
		t.Fatalf("PR reactivation error = %v", err)
	}

	if issueApp.Disposition != workflow.DispositionApplied || prApp.Disposition != workflow.DispositionApplied {
		t.Fatalf("reactivations = Issue %#v, PR %#v; want both APPLIED", issueApp, prApp)
	}
	if issueApp.State != workflow.StateDeveloping || prApp.State != workflow.StateDeveloping {
		t.Errorf("reactivation states = Issue %s, PR %s; want DEVELOPING", issueApp.State, prApp.State)
	}
	if issueApp.WorkflowID != issueWorkflowID || prApp.WorkflowID != prWorkflowID {
		t.Errorf("reactivation workflows = Issue %s, PR %s; want %s and %s", issueApp.WorkflowID, prApp.WorkflowID, issueWorkflowID, prWorkflowID)
	}
	for _, app := range []store.WorkflowApplication{issueApp, prApp} {
		var attempts int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_attempts WHERE workflow_id = $1 AND active`, app.WorkflowID).Scan(&attempts); err != nil {
			t.Fatalf("count active attempts: %v", err)
		}
		if attempts != 1 {
			t.Errorf("workflow %s active attempts = %d, want 1", app.WorkflowID, attempts)
		}
		var jobs, labels int
		if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE kind = 'PREPARE_AGENT_TURN'), count(*) FILTER (WHERE kind = 'RECONCILE_GITHUB_LABELS') FROM jobs WHERE workflow_id = $1 AND normalized_event_id = $2`, app.WorkflowID, app.DeliveryID).Scan(&jobs, &labels); err != nil {
			t.Fatalf("count reactivation jobs: %v", err)
		}
		if jobs != 1 || labels != 1 {
			t.Errorf("workflow %s jobs = prepare %d, labels %d; want 1 and 1", app.WorkflowID, jobs, labels)
		}
	}
}

// TestPRActivationResolvesAssociatedIssue verifies a PR trigger never creates a
// new Work Item from the PR number.
func TestPRActivationResolvesAssociatedIssue(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fixture := seedAgentSession(t, pool, 920)
	seedNeedsHumanWithProposal(t, pool, fixture.workflowID, fixture.attemptID, 920, 920, 920, 92000, 920, "durable-head")

	delivery := workflowDelivery("40000000-0000-4000-8000-000000000921")
	delivery.RepositoryID, delivery.RepositoryOwner, delivery.RepositoryName = 920, "owner", "repo"
	delivery.EventName, delivery.Action = "pull_request", "labeled"
	delivery.IssueID, delivery.IssueNumber = 0, 0
	claim := claimWorkflowDelivery(t, databases[0], ctx, delivery)
	application, err := databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "trigger"),
		store.WorkflowLocator{RepositoryID: 920, PullRequestID: 92000, PullRequestNumber: 920, PullRequestActivation: true},
		triggerEventFactory("50000000-0000-4000-8000-000000000921"))
	if err != nil {
		t.Fatalf("CompleteWebhookTransition() error = %v", err)
	}
	if application.WorkflowID != fixture.workflowID || application.Disposition != workflow.DispositionApplied {
		t.Fatalf("application = %#v, want applied to existing workflow %s", application, fixture.workflowID)
	}
	var workflows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflows`).Scan(&workflows); err != nil {
		t.Fatalf("count workflows: %v", err)
	}
	if workflows != 1 {
		t.Errorf("workflows = %d, want 1; PR number must not create a Work Item", workflows)
	}
	var issueID, issueNumber int64
	if err := pool.QueryRow(ctx, `SELECT issue_id, issue_number FROM workflows WHERE id = $1`, application.WorkflowID).Scan(&issueID, &issueNumber); err != nil {
		t.Fatalf("query Work Item: %v", err)
	}
	if issueID != 920 || issueNumber != 920 {
		t.Errorf("Work Item = (%d, %d), want associated Issue (920, 920)", issueID, issueNumber)
	}
}

// TestPRActivationRejectsNonCurrentProposals covers unknown, historical,
// marker-only, conflicting, cross-repository, and mismatched identities.
func TestPRActivationRejectsNonCurrentProposals(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fixture := seedAgentSession(t, pool, 930)
	seedNeedsHumanWithProposal(t, pool, fixture.workflowID, fixture.attemptID, 930, 930, 930, 93000, 930, "durable-head")
	// Replace the current proposal: the seeded row becomes historical.
	if _, err := pool.Exec(ctx, `UPDATE change_proposals SET active = FALSE WHERE workflow_id = $1`, fixture.workflowID); err != nil {
		t.Fatalf("deactivate current proposal: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO change_proposals (id, workflow_id, repository_id, repository_owner, repository_name, pull_request_id, pull_request_number, status, active, base_ref, base_sha, head_ref, head_sha) VALUES (gen_random_uuid(), $1, 930, 'owner', 'repo', 93001, 931, 'OPEN', TRUE, 'main', 'base', 'feature', 'current-head')`, fixture.workflowID); err != nil {
		t.Fatalf("seed current proposal: %v", err)
	}

	other := seedAgentSession(t, pool, 931)
	seedNeedsHumanWithProposal(t, pool, other.workflowID, other.attemptID, 931, 931, 931, 93100, 931, "other-head")

	tests := []struct {
		name    string
		locator store.WorkflowLocator
		wantErr bool
	}{
		{name: "unknown PR", locator: store.WorkflowLocator{RepositoryID: 930, PullRequestID: 99999, PullRequestNumber: 999, PullRequestActivation: true}},
		{name: "historical PR", locator: store.WorkflowLocator{RepositoryID: 930, PullRequestID: 93000, PullRequestNumber: 930, PullRequestActivation: true}},
		{name: "marker only", locator: store.WorkflowLocator{RepositoryID: 930, PullRequestID: 99998, PullRequestNumber: 998, WorkflowID: fixture.workflowID, PullRequestActivation: true}},
		{name: "mismatched number", locator: store.WorkflowLocator{RepositoryID: 930, PullRequestID: 93001, PullRequestNumber: 999, PullRequestActivation: true}},
		{name: "cross repository", locator: store.WorkflowLocator{RepositoryID: 999, PullRequestID: 93001, PullRequestNumber: 931, PullRequestActivation: true}},
		{name: "conflicting marker", locator: store.WorkflowLocator{RepositoryID: 930, PullRequestID: 93001, PullRequestNumber: 931, WorkflowID: other.workflowID, PullRequestActivation: true}, wantErr: true},
	}
	for index, test := range tests {
		deliveryID := "40000000-0000-4000-8000-00000000093" + string(rune('0'+index))
		delivery := workflowDelivery(deliveryID)
		delivery.RepositoryID, delivery.RepositoryOwner, delivery.RepositoryName = test.locator.RepositoryID, "owner", "repo"
		delivery.EventName, delivery.Action = "pull_request", "labeled"
		delivery.IssueID, delivery.IssueNumber = 0, 0
		claim := claimWorkflowDelivery(t, databases[0], ctx, delivery)
		application, err := databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
			normalizedPayload(claim.DeliveryID, "trigger"), test.locator,
			triggerEventFactory("50000000-0000-4000-8000-00000000093"+string(rune('0'+index))))
		if test.wantErr {
			if !errors.Is(err, store.ErrWorkflowLocatorMismatch) {
				t.Errorf("%s error = (%v, %#v), want ErrWorkflowLocatorMismatch", test.name, err, application)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s error = %v, want unrelated completion", test.name, err)
			continue
		}
		if application.Disposition != workflow.DispositionUnrelated {
			t.Errorf("%s disposition = %s, want UNRELATED without activation", test.name, application.Disposition)
		}
		if application.Status != store.NormalizedEventCompleted {
			t.Errorf("%s status = %s, want COMPLETED", test.name, application.Status)
		}
	}
	var activeAttempts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_attempts WHERE active AND workflow_id = $1`, fixture.workflowID).Scan(&activeAttempts); err != nil {
		t.Fatalf("count active attempts: %v", err)
	}
	if activeAttempts != 1 {
		t.Errorf("active attempts after rejected PR commands = %d, want 1", activeAttempts)
	}
}

// TestPRActivationDoesNotCreateWorkflow verifies an unresolved PR trigger never
// enters the absent-Workflow creation path.
func TestPRActivationDoesNotCreateWorkflow(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	delivery := workflowDelivery("40000000-0000-4000-8000-000000000940")
	delivery.RepositoryID, delivery.RepositoryOwner, delivery.RepositoryName = 940, "owner", "repo"
	delivery.EventName, delivery.Action = "pull_request", "labeled"
	delivery.IssueID, delivery.IssueNumber = 0, 0
	claim := claimWorkflowDelivery(t, databases[0], ctx, delivery)
	application, err := databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "trigger"),
		store.WorkflowLocator{RepositoryID: 940, PullRequestID: 94000, PullRequestNumber: 940, PullRequestActivation: true},
		triggerEventFactory("50000000-0000-4000-8000-000000000940"))
	if err != nil {
		t.Fatalf("CompleteWebhookTransition() error = %v", err)
	}
	if application.Disposition != workflow.DispositionUnrelated || application.WorkflowID != "" {
		t.Fatalf("application = %#v, want unrelated without Workflow", application)
	}
	var workflows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflows`).Scan(&workflows); err != nil {
		t.Fatalf("count workflows: %v", err)
	}
	if workflows != 0 {
		t.Errorf("workflows = %d, want 0; PR activation must not create a Work Item", workflows)
	}
}

// TestPRActivationPreservesHead ensures label webhook revision data cannot
// rewind the durable Change Proposal head.
func TestPRActivationPreservesHead(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fixture := seedAgentSession(t, pool, 950)
	seedNeedsHumanWithProposal(t, pool, fixture.workflowID, fixture.attemptID, 950, 950, 950, 95000, 950, "durable-head-v2")

	delivery := workflowDelivery("40000000-0000-4000-8000-000000000951")
	delivery.RepositoryID, delivery.RepositoryOwner, delivery.RepositoryName = 950, "owner", "repo"
	delivery.EventName, delivery.Action = "pull_request", "labeled"
	delivery.IssueID, delivery.IssueNumber = 0, 0
	claim := claimWorkflowDelivery(t, databases[0], ctx, delivery)
	application, err := databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "trigger"),
		store.WorkflowLocator{RepositoryID: 950, PullRequestID: 95000, PullRequestNumber: 950, PullRequestActivation: true},
		func(context store.WorkflowEventContext) (workflow.Event, error) {
			return workflow.TriggerEvent{
				EventMetadata: context.Metadata,
				AttemptID:     "50000000-0000-4000-8000-000000000951", AttemptNumber: context.Snapshot.LastAttemptNumber + 1,
			}, nil
		})
	if err != nil || application.Disposition != workflow.DispositionApplied {
		t.Fatalf("PR activation = (%#v, %v), want applied", application, err)
	}
	var head string
	if err := pool.QueryRow(ctx, `SELECT head_sha FROM change_proposals WHERE workflow_id = $1 AND active`, fixture.workflowID).Scan(&head); err != nil {
		t.Fatalf("query durable head: %v", err)
	}
	if head != "durable-head-v2" {
		t.Errorf("durable head = %q, want durable-head-v2; label payload must not rewind", head)
	}
	var expectedHead string
	if err := pool.QueryRow(ctx, `SELECT payload->>'expected_head_sha' FROM jobs WHERE workflow_id = $1 AND kind = 'PREPARE_AGENT_TURN' AND normalized_event_id = $2`, fixture.workflowID, claim.DeliveryID).Scan(&expectedHead); err != nil {
		t.Fatalf("query successor head: %v", err)
	}
	if expectedHead != "durable-head-v2" {
		t.Errorf("successor expected head = %q, want durable head", expectedHead)
	}
}

// TestPRActivationDuplicateDeliveryIsIdempotent verifies redelivery applies at
// most once via delivery-ID deduplication.
func TestPRActivationDuplicateDeliveryIsIdempotent(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fixture := seedAgentSession(t, pool, 960)
	seedNeedsHumanWithProposal(t, pool, fixture.workflowID, fixture.attemptID, 960, 960, 960, 96000, 960, "head")

	delivery := workflowDelivery("40000000-0000-4000-8000-000000000961")
	delivery.RepositoryID, delivery.RepositoryOwner, delivery.RepositoryName = 960, "owner", "repo"
	delivery.EventName, delivery.Action = "pull_request", "labeled"
	delivery.IssueID, delivery.IssueNumber = 0, 0
	if inserted, err := databases[0].InsertWebhookDelivery(ctx, delivery); err != nil || !inserted {
		t.Fatalf("InsertWebhookDelivery() = (%t, %v), want inserted", inserted, err)
	}
	if inserted, err := databases[0].InsertWebhookDelivery(ctx, delivery); err != nil || inserted {
		t.Fatalf("duplicate InsertWebhookDelivery() = (%t, %v), want deduplicated", inserted, err)
	}
	claim, err := databases[0].ClaimWebhookDelivery(ctx, "processor", 30*time.Second)
	if err != nil || claim == nil {
		t.Fatalf("ClaimWebhookDelivery() = (%#v, %v), want claim", claim, err)
	}
	application, err := databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "trigger"),
		store.WorkflowLocator{RepositoryID: 960, PullRequestID: 96000, PullRequestNumber: 960, PullRequestActivation: true},
		triggerEventFactory("50000000-0000-4000-8000-000000000961"))
	if err != nil || application.Disposition != workflow.DispositionApplied {
		t.Fatalf("first PR activation = (%#v, %v), want applied", application, err)
	}
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_attempts WHERE workflow_id = $1`, fixture.workflowID).Scan(&attempts); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts after duplicate delivery = %d, want 2 (initial + one reactivation)", attempts)
	}
}

// TestConcurrentIssueAndPRTriggersCreateOneAttempt ensures competing human
// commands serialize on the Workflow lock without duplicate successor work.
func TestConcurrentIssueAndPRTriggersCreateOneAttempt(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fixture := seedAgentSession(t, pool, 970)
	seedNeedsHumanWithProposal(t, pool, fixture.workflowID, fixture.attemptID, 970, 970, 970, 97000, 970, "head")

	issueDelivery := workflowDelivery("40000000-0000-4000-8000-000000000971")
	issueDelivery.RepositoryID, issueDelivery.RepositoryOwner, issueDelivery.RepositoryName = 970, "owner", "repo"
	issueDelivery.IssueID, issueDelivery.IssueNumber = 970, 970
	prDelivery := workflowDelivery("40000000-0000-4000-8000-000000000972")
	prDelivery.RepositoryID, prDelivery.RepositoryOwner, prDelivery.RepositoryName = 970, "owner", "repo"
	prDelivery.EventName, prDelivery.Action = "pull_request", "labeled"
	prDelivery.IssueID, prDelivery.IssueNumber = 0, 0
	for _, delivery := range []store.WebhookDelivery{issueDelivery, prDelivery} {
		if inserted, err := databases[0].InsertWebhookDelivery(ctx, delivery); err != nil || !inserted {
			t.Fatalf("insert %s = (%t, %v)", delivery.DeliveryID, inserted, err)
		}
	}
	claims := make([]*store.WebhookClaim, 0, 2)
	for range 2 {
		claim, err := databases[0].ClaimWebhookDelivery(ctx, "concurrent-worker", 30*time.Second)
		if err != nil || claim == nil {
			t.Fatalf("claim concurrent delivery = (%#v, %v)", claim, err)
		}
		claims = append(claims, claim)
	}
	type result struct {
		application store.WorkflowApplication
		err         error
	}
	results := make(chan result, 2)
	var wait sync.WaitGroup
	for index, claim := range claims {
		wait.Add(1)
		go func(index int, claim *store.WebhookClaim) {
			defer wait.Done()
			var application store.WorkflowApplication
			var err error
			if claim.DeliveryID == issueDelivery.DeliveryID {
				application, err = databases[index].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
					normalizedPayload(claim.DeliveryID, "trigger"),
					store.WorkflowLocator{RepositoryID: 970, IssueID: 970, IssueNumber: 970},
					triggerEventFactory("50000000-0000-4000-8000-000000000971"))
			} else {
				application, err = databases[index].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
					normalizedPayload(claim.DeliveryID, "trigger"),
					store.WorkflowLocator{RepositoryID: 970, PullRequestID: 97000, PullRequestNumber: 970, PullRequestActivation: true},
					triggerEventFactory("50000000-0000-4000-8000-000000000972"))
			}
			results <- result{application: application, err: err}
		}(index, claim)
	}
	wait.Wait()
	close(results)
	dispositions := map[string]int{}
	for item := range results {
		if item.err != nil {
			t.Fatalf("concurrent trigger error = %v", item.err)
		}
		dispositions[string(item.application.Disposition)]++
		if item.application.WorkflowID != fixture.workflowID {
			t.Errorf("concurrent application workflow = %s, want %s", item.application.WorkflowID, fixture.workflowID)
		}
	}
	if dispositions[string(workflow.DispositionApplied)] != 1 || dispositions[string(workflow.DispositionDuplicate)] != 1 {
		t.Errorf("concurrent dispositions = %#v, want one APPLIED and one DUPLICATE", dispositions)
	}
	var active int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_attempts WHERE workflow_id = $1 AND active`, fixture.workflowID).Scan(&active); err != nil {
		t.Fatalf("count active attempts: %v", err)
	}
	if active != 1 {
		t.Errorf("active attempts = %d, want 1", active)
	}
}

// TestPRActivationDefersDuringActiveTurn verifies active-Turn deferral.
func TestPRActivationDefersDuringActiveTurn(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 980)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE workflows SET status = 'DEVELOPING', desired_assignment_status = 'ACTIVE', desired_runtime_state = 'ACTIVE' WHERE id = $1`, fixture.workflowID); err != nil {
		t.Fatalf("make DEVELOPING: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit = 1, current_stage = 'implementation', review_usage = '{}'::jsonb WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatalf("set attempt: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO change_proposals (id, workflow_id, repository_id, repository_owner, repository_name, pull_request_id, pull_request_number, status, base_ref, base_sha, head_ref, head_sha) VALUES (gen_random_uuid(), $1, 980, 'owner', 'repo', 98000, 980, 'OPEN', 'main', 'base', 'feature', 'head')`, fixture.workflowID); err != nil {
		t.Fatalf("seed proposal: %v", err)
	}
	turnSpec := fixture.turnSpec()
	turnSpec.ExpectedHeadSHA = "head"
	turn, err := prepareFixtureAgentTurn(t, databases[0], pool, ctx, turnSpec)
	if err != nil {
		t.Fatalf("PrepareAgentTurn() error = %v", err)
	}
	delivery := workflowDelivery("40000000-0000-4000-8000-000000000981")
	delivery.RepositoryID, delivery.RepositoryOwner, delivery.RepositoryName = 980, "owner", "repo"
	delivery.EventName, delivery.Action = "pull_request", "labeled"
	delivery.IssueID, delivery.IssueNumber = 0, 0
	claim := claimWorkflowDelivery(t, databases[0], ctx, delivery)
	application, err := databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "trigger"),
		store.WorkflowLocator{RepositoryID: 980, PullRequestID: 98000, PullRequestNumber: 980, PullRequestActivation: true},
		triggerEventFactory("50000000-0000-4000-8000-000000000981"))
	if err != nil {
		t.Fatalf("CompleteWebhookTransition() error = %v", err)
	}
	if application.Status != store.NormalizedEventDeferred || application.Disposition != workflow.DispositionDeferred {
		t.Fatalf("application = %#v, want DEFERRED during active turn", application)
	}
	event, err := databases[0].GetNormalizedEvent(ctx, claim.DeliveryID)
	if err != nil {
		t.Fatalf("GetNormalizedEvent() error = %v", err)
	}
	if event.DeferredForTurnID != turn.ID {
		t.Errorf("deferred turn = %q, want %s", event.DeferredForTurnID, turn.ID)
	}
}

// TestPRActivationDeferredReplaySettlesThroughReconciliation replays a deferred
// PR activation through AcknowledgePendingEventReconciliation. The eligible
// replay creates the next Attempt through the shared trigger path, while a
// proposal change before replay completes the event durably as UNRELATED.
func TestPRActivationDeferredReplaySettlesThroughReconciliation(t *testing.T) {
	t.Run("eligible replay creates next attempt", func(t *testing.T) {
		settleDeferredPRActivation(t, 987, false)
	})
	t.Run("proposal change before replay is unrelated", func(t *testing.T) {
		settleDeferredPRActivation(t, 988, true)
	})
}

func settleDeferredPRActivation(t *testing.T, seedNumber int, replaceProposal bool) {
	t.Helper()
	databases, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	repository := int64(seedNumber)
	prID := repository * 100
	prNumber := repository
	const head = "head"

	// Follow the production Agent Turn lifecycle: prepare and acquire a Reviewer
	// turn bound to the current proposal, defer the PR activation through the
	// webhook seam while the turn is active, then settle through SettleAgentTurn
	// so terminal completion, the Workflow transition, and the linked
	// RECONCILE_PENDING_EVENTS job are created with production barriers.
	fixture, lease, _ := prepareSettlementTurn(t, databases[0], pool, ctx, seedNumber, workflow.RoleReviewer, head)

	prDeliveryID := fmt.Sprintf("40000000-0000-4000-8000-%012d", seedNumber*1000+1)
	prDelivery := workflowDelivery(prDeliveryID)
	prDelivery.RepositoryID, prDelivery.RepositoryOwner, prDelivery.RepositoryName = repository, "owner", "repo"
	prDelivery.EventName, prDelivery.Action = "pull_request", "labeled"
	prDelivery.IssueID, prDelivery.IssueNumber = 0, 0
	prClaim := claimWorkflowDelivery(t, databases[0], ctx, prDelivery)
	prAttemptID := fmt.Sprintf("50000000-0000-4000-8000-%012d", seedNumber*1000+1)
	prApplication, err := databases[0].CompleteWebhookTransition(ctx, prClaim.DeliveryID, prClaim.ClaimToken,
		normalizedPayload(prClaim.DeliveryID, "trigger"),
		store.WorkflowLocator{RepositoryID: repository, PullRequestID: prID, PullRequestNumber: prNumber, PullRequestActivation: true},
		triggerEventFactory(prAttemptID))
	if err != nil {
		t.Fatalf("defer PR activation error = %v", err)
	}
	if prApplication.Status != store.NormalizedEventDeferred || prApplication.Disposition != workflow.DispositionDeferred {
		t.Fatalf("PR activation = %#v, want DEFERRED during active turn", prApplication)
	}

	observation := successfulSettlementObservation(workflow.TurnOutcomeBlocked, nil)
	observation.Diagnostic = "blocked for deferred PR replay"
	settled, err := databases[0].SettleAgentTurn(ctx, lease, observation)
	if err != nil {
		t.Fatalf("SettleAgentTurn() blocked error = %v", err)
	}
	if settled.PendingEventCount != 1 || settled.ReconciliationJobID == "" || settled.SuccessorJobID != "" {
		t.Fatalf("blocked settlement = %#v, want one pending event with reconciliation and no direct successor", settled)
	}
	if settled.State != workflow.StateNeedsHuman {
		t.Fatalf("blocked settlement state = %s, want NEEDS_HUMAN", settled.State)
	}

	if replaceProposal {
		if _, err := pool.Exec(ctx, `UPDATE change_proposals SET active = FALSE WHERE workflow_id = $1`, fixture.workflowID); err != nil {
			t.Fatalf("deactivate old proposal: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO change_proposals (id, workflow_id, repository_id, repository_owner, repository_name, pull_request_id, pull_request_number, status, active, base_ref, base_sha, head_ref, head_sha) VALUES (gen_random_uuid(), $1, $2, 'owner', 'repo', $3, $4, 'OPEN', TRUE, 'main', 'base', 'feature', 'head-v2')`,
			fixture.workflowID, repository, prID+1, prNumber+1); err != nil {
			t.Fatalf("seed replacement proposal: %v", err)
		}
	}

	job, err := databases[0].ClaimJobKind(ctx, store.WorkflowActionQueue, store.ReconcilePendingEventsJobKind, "reconciliation-worker", 10*time.Second)
	if err != nil || job == nil {
		t.Fatalf("ClaimJobKind() reconciliation = (%#v, %v)", job, err)
	}
	replayAttemptID := fmt.Sprintf("50000000-0000-4000-8000-%012d", seedNumber*1000+3)
	factory := func(record store.NormalizedEventRecord) (store.WorkflowLocator, store.WorkflowEventFactory, error) {
		if record.DeliveryID != prDeliveryID {
			return store.WorkflowLocator{}, nil, errors.New("unexpected deferred event replayed")
		}
		locator := store.WorkflowLocator{RepositoryID: repository, PullRequestID: prID, PullRequestNumber: prNumber, PullRequestActivation: true}
		return locator, triggerEventFactory(replayAttemptID), nil
	}
	acknowledged, err := databases[0].AcknowledgePendingEventReconciliation(ctx, *job, factory)
	if err != nil {
		t.Fatalf("AcknowledgePendingEventReconciliation() error = %v", err)
	}
	if acknowledged.CompletedCount != 1 || acknowledged.SourceTurnID != lease.ID {
		t.Fatalf("reconciliation acknowledgement = %#v, want one event from turn %s", acknowledged, lease.ID)
	}

	event, err := databases[0].GetNormalizedEvent(ctx, prDeliveryID)
	if err != nil {
		t.Fatalf("GetNormalizedEvent() error = %v", err)
	}
	if replaceProposal {
		if event.Status != store.NormalizedEventCompleted || event.Disposition != workflow.DispositionUnrelated || string(event.Reason) != string(workflow.ReasonChangeProposalUnrelated) {
			t.Errorf("replayed event = %#v, want COMPLETED UNRELATED change_proposal_unrelated", event)
		}
		if acknowledged.SuccessorJobID != "" {
			t.Errorf("successor job = %q, want none for unrelated replay", acknowledged.SuccessorJobID)
		}
		var attempts int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_attempts WHERE workflow_id = $1`, fixture.workflowID).Scan(&attempts); err != nil {
			t.Fatalf("count attempts: %v", err)
		}
		if attempts != 1 {
			t.Errorf("attempts after unrelated replay = %d, want 1", attempts)
		}
		return
	}
	if event.Status != store.NormalizedEventCompleted || event.Disposition != workflow.DispositionApplied || string(event.Reason) != string(workflow.ReasonTriggered) {
		t.Errorf("replayed event = %#v, want COMPLETED APPLIED workflow_triggered", event)
	}
	if acknowledged.Successor.Role != workflow.RoleReviewer || acknowledged.Successor.Purpose != workflow.TurnPurposeReactivation || acknowledged.SuccessorJobID == "" {
		t.Errorf("successor = %#v, want Reviewer reactivation with job", acknowledged.Successor)
	}
	var state string
	var activeAttempts int
	if err := pool.QueryRow(ctx, `SELECT status FROM workflows WHERE id = $1`, fixture.workflowID).Scan(&state); err != nil {
		t.Fatalf("query workflow: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_attempts WHERE workflow_id = $1 AND active`, fixture.workflowID).Scan(&activeAttempts); err != nil {
		t.Fatalf("count active attempts: %v", err)
	}
	if state != string(workflow.StateReviewing) || activeAttempts != 1 {
		t.Errorf("workflow after eligible replay = %s with %d active attempts, want REVIEWING with 1", state, activeAttempts)
	}
	var lastAttempt uint64
	if err := pool.QueryRow(ctx, `SELECT MAX(attempt_number) FROM workflow_attempts WHERE workflow_id = $1`, fixture.workflowID).Scan(&lastAttempt); err != nil {
		t.Fatalf("query attempt sequence: %v", err)
	}
	if lastAttempt != 2 {
		t.Errorf("last attempt number = %d, want 2", lastAttempt)
	}
}

// TestPRActivationRechecksEligibilityOnPendingReplay verifies a PENDING PR
// command is rechecked when the current proposal changes before replay.
func TestPRActivationRechecksEligibilityOnPendingReplay(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fixture := seedAgentSession(t, pool, 985)
	seedNeedsHumanWithProposal(t, pool, fixture.workflowID, fixture.attemptID, 985, 985, 985, 98500, 985, "head-v1")

	delivery := workflowDelivery("40000000-0000-4000-8000-000000000986")
	delivery.RepositoryID, delivery.RepositoryOwner, delivery.RepositoryName = 985, "owner", "repo"
	delivery.EventName, delivery.Action = "pull_request", "labeled"
	delivery.IssueID, delivery.IssueNumber = 0, 0
	claim := claimWorkflowDelivery(t, databases[0], ctx, delivery)
	prPayload := jsonMarshalPRActivation(map[string]string{"delivery_id": claim.DeliveryID, "kind": "trigger"})
	if err := databases[0].CompleteWebhookDelivery(ctx, claim.DeliveryID, claim.ClaimToken, store.WebhookCompletion{
		Outcome: store.WebhookOutcomeProcessed, NormalizedPayload: prPayload,
	}); err != nil {
		t.Fatalf("seed pending PR activation: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE webhook_deliveries SET workflow_id = $1 WHERE delivery_id = $2`, fixture.workflowID, claim.DeliveryID); err != nil {
		t.Fatalf("bind pending delivery: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE change_proposals SET active = FALSE WHERE workflow_id = $1`, fixture.workflowID); err != nil {
		t.Fatalf("deactivate old proposal: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO change_proposals (id, workflow_id, repository_id, repository_owner, repository_name, pull_request_id, pull_request_number, status, active, base_ref, base_sha, head_ref, head_sha) VALUES (gen_random_uuid(), $1, 985, 'owner', 'repo', 98501, 986, 'OPEN', TRUE, 'main', 'base', 'feature', 'head-v2')`, fixture.workflowID); err != nil {
		t.Fatalf("seed replacement proposal: %v", err)
	}

	application, applied, err := databases[0].ApplyNextPendingNormalizedEvent(ctx, func(record store.NormalizedEventRecord) (store.WorkflowLocator, store.WorkflowEventFactory, error) {
		locator := store.WorkflowLocator{RepositoryID: 985, PullRequestID: 98500, PullRequestNumber: 985, PullRequestActivation: true}
		return locator, triggerEventFactory("50000000-0000-4000-8000-000000000986"), nil
	})
	if err != nil {
		t.Fatalf("ApplyNextPendingNormalizedEvent() error = %v", err)
	}
	if !applied {
		t.Fatal("ApplyNextPendingNormalizedEvent() applied = false, want replayed")
	}
	if application.Disposition != workflow.DispositionUnrelated {
		t.Errorf("replayed disposition = %s, want UNRELATED after proposal replacement", application.Disposition)
	}
	var active int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_attempts WHERE workflow_id = $1 AND active`, fixture.workflowID).Scan(&active); err != nil {
		t.Fatalf("count active attempts: %v", err)
	}
	if active != 1 {
		t.Errorf("active attempts after stale replay = %d, want 1", active)
	}
}

// TestPRActivationPreservesClosedLifecycle ensures a PR command never reopens
// the Issue and respects closure ordering.
func TestPRActivationPreservesClosedLifecycle(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fixture := seedAgentSession(t, pool, 990)
	seedNeedsHumanWithProposal(t, pool, fixture.workflowID, fixture.attemptID, 990, 990, 990, 99000, 990, "head")
	if _, err := pool.Exec(ctx, `UPDATE workflows SET status = 'CLOSED', state_revision = 5, desired_assignment_status = 'COMPLETED', desired_runtime_state = 'RETAINED', resume_role = NULL, retention_deadline = clock_timestamp() + interval '30 days', retention_token = 'retention-990' WHERE id = $1`, fixture.workflowID); err != nil {
		t.Fatalf("make CLOSED: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET active = FALSE, status = 'ISSUE_CLOSED' WHERE id = $1`, fixture.attemptID); err != nil {
		t.Fatalf("close attempt: %v", err)
	}

	delivery := workflowDelivery("40000000-0000-4000-8000-000000000991")
	delivery.RepositoryID, delivery.RepositoryOwner, delivery.RepositoryName = 990, "owner", "repo"
	delivery.EventName, delivery.Action = "pull_request", "labeled"
	delivery.IssueID, delivery.IssueNumber = 0, 0
	claim := claimWorkflowDelivery(t, databases[0], ctx, delivery)
	application, err := databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "trigger"),
		store.WorkflowLocator{RepositoryID: 990, PullRequestID: 99000, PullRequestNumber: 990, PullRequestActivation: true},
		triggerEventFactory("50000000-0000-4000-8000-000000000991"))
	if err != nil {
		t.Fatalf("CompleteWebhookTransition() error = %v", err)
	}
	if application.Disposition == workflow.DispositionApplied {
		t.Fatalf("application = %#v, want no new Attempt from CLOSED", application)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM workflows WHERE id = $1`, fixture.workflowID).Scan(&status); err != nil {
		t.Fatalf("query workflow: %v", err)
	}
	if status != "CLOSED" {
		t.Errorf("workflow status = %s, want CLOSED preserved", status)
	}
}
