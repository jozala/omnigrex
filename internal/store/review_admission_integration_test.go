//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/agentturn"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestReviewerAllocationRequiresControllerAdmission(t *testing.T) {
	database, pool, ctx, lease := reviewAdmissionFixture(t)
	_, err := database.PrepareAgentTurn(ctx, lease, preparationSpec("profile-set", "openai/developer"))
	if err == nil {
		t.Fatal("Reviewer Turn allocated without a controller CI admission request")
	}
	var turns, executions int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM agent_turns WHERE workflow_id = $1), (SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind = 'RUN_AGENT_TURN')`, lease.WorkflowID).Scan(&turns, &executions); err != nil {
		t.Fatal(err)
	}
	if turns != 0 || executions != 0 {
		t.Fatalf("unadmitted Reviewer allocated %d Turns and %d execution jobs", turns, executions)
	}
}

func reviewAdmissionFixture(t *testing.T) (*store.Store, *pgxpool.Pool, context.Context, store.JobLease) {
	t.Helper()
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	application := triggerPreparationWorkflow(t, database, ctx, "61000000-0000-4000-8000-000000000010", "62000000-0000-4000-8000-000000000010")
	if _, err := pool.Exec(ctx, `UPDATE workflows SET status = 'REVIEWING' WHERE id = $1`, application.WorkflowID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET current_stage = 'review' WHERE workflow_id = $1 AND active`, application.WorkflowID); err != nil {
		t.Fatal(err)
	}
	proposalID := "63000000-0000-4000-8000-000000000010"
	if _, err := pool.Exec(ctx, `
INSERT INTO change_proposals (
    id, workflow_id, repository_id, repository_owner, repository_name,
    pull_request_id, pull_request_number, status, base_ref, base_sha, head_ref, head_sha
)
VALUES ($1, $2, 9123, 'owner', 'repo', 10, 10, 'OPEN', 'main', 'base', 'feature', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa')`,
		proposalID, application.WorkflowID); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{
		"mode": workflow.AssignmentGenerationNew, "stage": workflow.StageReview, "role": workflow.RoleReviewer,
		"purpose": workflow.TurnPurposeReview, "expected_head_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"retry_of_turn_id": "", "revision": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET payload = $2 WHERE workflow_id = $1 AND kind = 'PREPARE_AGENT_TURN'`, application.WorkflowID, payload); err != nil {
		t.Fatal(err)
	}

	lease := claimPreparationJob(t, database, ctx)
	return database, pool, ctx, lease
}

// This uses #122's production policy discovery, status/check readers and verified
// test-merge mapping against a deterministic provider, not a gate-only CI reader.
func TestReviewGateSharedObserverPersistsAndFencesTargets(t *testing.T) {
	database, pool, ctx, lease := reviewAdmissionFixture(t)
	provider := &admissionProvider{head: strings.Repeat("a", 40), base: strings.Repeat("b", 40), merge: strings.Repeat("c", 40), pending: true}
	server := httptest.NewServer(provider)
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	gate := &agentturn.ReviewGate{Store: database, Source: workflowQualificationProfiles{head: strings.Repeat("d", 40)}, Observer: client, Now: func() time.Time { return now }}
	request, err := database.BeginReviewAdmission(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	originalDeadline := request.Deadline
	for n := 0; n < 5; n++ {
		request, err = gate.Check(ctx, agentturn.Request{Lease: lease, InstallationCredential: "fixture", RepositoryOwner: "owner", RepositoryName: "repo"}, request)
		if err != nil {
			t.Fatal(err)
		}
		if request.Status != "WAITING" {
			t.Fatalf("pending provider released: %+v", request)
		}
		if err = database.DeferReviewAdmission(ctx, lease, time.Second); err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, `UPDATE jobs SET available_at=clock_timestamp() WHERE id=$1`, lease.ID); err != nil {
			t.Fatal(err)
		}
		lease = claimPreparationJob(t, database, ctx)
		request, err = database.BeginReviewAdmission(ctx, lease)
		if err != nil {
			t.Fatal(err)
		}
	}
	var turns, participants, infra int
	if err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM agent_turns WHERE workflow_id=$1),(SELECT count(*) FROM agent_participants WHERE workflow_id=$1),(SELECT infrastructure_failures FROM workflow_attempts WHERE workflow_id=$1 AND active)`, lease.WorkflowID).Scan(&turns, &participants, &infra); err != nil {
		t.Fatal(err)
	}
	if turns != 0 || participants != 0 || infra != 0 {
		t.Fatalf("healthy wait spent budgets or runtime allocation: %d %d %d", turns, participants, infra)
	}
	if provider.discoveryCount() != 1 {
		t.Fatalf("inventory rediscovered %d times", provider.discoveryCount())
	}
	// Reconstruct controller object, retaining the database request and deadline.
	gate = &agentturn.ReviewGate{Store: database, Source: workflowQualificationProfiles{head: strings.Repeat("e", 40)}, Observer: client, Now: func() time.Time { return now }}
	old := request
	provider.mu.Lock()
	provider.base = strings.Repeat("e", 40)
	provider.merge = strings.Repeat("f", 40)
	provider.mu.Unlock()
	request, err = gate.Check(ctx, agentturn.Request{Lease: lease, InstallationCredential: "fixture", RepositoryOwner: "owner", RepositoryName: "repo"}, request)
	if err != nil {
		t.Fatal(err)
	}
	if request.Generation != old.Generation+1 || request.Deadline != originalDeadline || request.PolicySourceSHA != strings.Repeat("d", 40) {
		t.Fatalf("retarget lost snapshot/deadline: %+v", request)
	}
	if _, err = database.RecordReviewAdmission(ctx, lease, old.ID, store.ReviewAdmissionObservation{Generation: old.Generation, HeadSHA: old.HeadSHA, Release: true, Evidence: json.RawMessage(`{"release_reason":"completed_ci"}`)}); !errors.Is(err, store.ErrReviewAdmissionPending) {
		t.Fatalf("stale generation accepted: %v", err)
	}
	provider.mu.Lock()
	provider.pending = false
	provider.mu.Unlock()
	if _, err := client.GetCommitStatuses(ctx, "fixture", "owner", "repo", provider.merge); err != nil {
		t.Fatalf("status fixture: %v", err)
	}
	request, err = gate.Check(ctx, agentturn.Request{Lease: lease, InstallationCredential: "fixture", RepositoryOwner: "owner", RepositoryName: "repo"}, request)
	if err != nil {
		t.Fatal(err)
	}
	if request.Status != "RELEASED" || !strings.Contains(string(request.ReleaseEvidence), "terminal_non_success") {
		t.Fatalf("completed failure did not release: %+v", request)
	}
	spec := preparationSpec("profile", "provider/model")
	spec.ReviewAdmission = &request
	prepared, err := database.PrepareAgentTurn(ctx, lease, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prepared.Turn.TurnConfiguration), "review_start") || !strings.Contains(string(prepared.Turn.TurnConfiguration), provider.merge) {
		t.Fatal("admitted context lost actual tested merge target")
	}
	if _, err = database.PrepareAgentTurn(ctx, lease, spec); err == nil {
		t.Fatal("duplicate admission allocated another Turn")
	}
}

type admissionProvider struct {
	mu                   sync.Mutex
	head, base, merge    string
	pending              bool
	discoveryUnavailable bool
	discoveries          int
}

func (p *admissionProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	root := "/repos/owner/repo"
	switch r.URL.Path {
	case root:
		fmt.Fprint(w, `{"id":9123,"name":"repo","full_name":"owner/repo","owner":{"login":"owner"}}`)
	case root + "/pulls/10":
		fmt.Fprintf(w, `{"id":10,"node_id":"PR_10","number":10,"title":"fixture","state":"open","html_url":"https://github.test/owner/repo/pull/10","head":{"ref":"feature","sha":%q},"base":{"ref":"main","sha":%q,"repo":{"id":9123,"name":"repo","full_name":"owner/repo","owner":{"login":"owner"}}},"mergeable":true,"merge_commit_sha":%q}`, p.head, p.base, p.merge)
	case root + "/branches/main":
		fmt.Fprintf(w, `{"name":"main","protected":true,"commit":{"sha":%q}}`, p.base)
	case root + "/branches/main/protection":
		p.discoveries++
		if p.discoveryUnavailable {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
			return
		}
		fmt.Fprint(w, `{"required_status_checks":{"contexts":["build"],"checks":[]}}`)
	case root + "/rules/branches/main":
		fmt.Fprint(w, `[]`)
	case root + "/git/commits/" + p.merge:
		fmt.Fprintf(w, `{"sha":%q,"parents":[{"sha":%q},{"sha":%q}]}`, p.merge, p.base, p.head)
	case root + "/commits/" + p.head + "/check-runs", root + "/commits/" + p.merge + "/check-runs":
		fmt.Fprint(w, `{"total_count":0,"check_runs":[]}`)
	case root + "/commits/" + p.head + "/statuses":
		fmt.Fprint(w, `[]`)
	case root + "/commits/" + p.merge + "/statuses":
		state := "failure"
		if p.pending {
			state = "pending"
		}
		fmt.Fprintf(w, `[{"id":1,"node_id":"S_1","context":"build","state":%q,"url":"https://api.github.test/repos/owner/repo/statuses/%s","created_at":"2026-10-27T12:00:00Z","updated_at":"2026-10-27T12:00:00Z"}]`, state, p.merge)
	default:
		http.NotFound(w, r)
	}
}

func TestReviewAdmissionCoalescesSynchronizationAndCancelsOnClosure(t *testing.T) {
	database, pool, ctx, lease := reviewAdmissionFixture(t)
	request, err := database.BeginReviewAdmission(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	request, err = database.RecordReviewAdmission(ctx, lease, request.ID, store.ReviewAdmissionObservation{Generation: request.Generation, HeadSHA: request.HeadSHA, Policy: json.RawMessage(`{"start":"after_ci","selection":"github_required","timeout":1800000000000,"registration_grace":60000000000}`), Inventory: json.RawMessage(`{"availability":"unavailable"}`), Evidence: json.RawMessage(`{"release_reason":"waiting"}`)})
	if err != nil {
		t.Fatal(err)
	}
	deadline := request.Deadline
	delivery := workflowDelivery("61000000-0000-4000-8000-000000000011")
	delivery.IssueID = 0
	delivery.IssueNumber = 0
	claim := claimWorkflowDelivery(t, database, ctx, delivery)
	application, err := database.CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken, normalizedPayload(claim.DeliveryID, "synchronize"), store.WorkflowLocator{RepositoryID: 9123, PullRequestID: 10, PullRequestNumber: 10}, func(c store.WorkflowEventContext) (workflow.Event, error) {
		return workflow.SynchronizationEvent{EventMetadata: c.Metadata, ChangeProposalID: 10, PreviousHeadSHA: request.HeadSHA, HeadSHA: strings.Repeat("b", 40)}, nil
	})
	if err != nil || application.Disposition != workflow.DispositionApplied {
		t.Fatalf("synchronization: %+v %v", application, err)
	}
	var count int
	var jobID string
	if err = pool.QueryRow(ctx, `SELECT count(*),min(id::text) FROM jobs WHERE workflow_id=$1 AND kind='PREPARE_AGENT_TURN' AND status IN ('AVAILABLE','LEASED')`, lease.WorkflowID).Scan(&count, &jobID); err != nil {
		t.Fatal(err)
	}
	if count != 1 || jobID != lease.ID {
		t.Fatalf("synchronization created competing jobs: %d %s", count, jobID)
	}
	replacement, err := database.BeginReviewAdmission(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID != request.ID || replacement.Generation != request.Generation+1 || replacement.HeadSHA != strings.Repeat("b", 40) || !replacement.Deadline.Equal(deadline) || len(replacement.ReleaseEvidence) > 0 {
		t.Fatalf("coalesced request lost target fence: %+v", replacement)
	}
	if _, err = database.RecordReviewAdmission(ctx, lease, request.ID, store.ReviewAdmissionObservation{Generation: request.Generation, HeadSHA: request.HeadSHA, Release: true, Evidence: json.RawMessage(`{"release_reason":"completed_ci"}`)}); !errors.Is(err, store.ErrReviewAdmissionPending) {
		t.Fatalf("old target released: %v", err)
	}
	closeDelivery := workflowDelivery("61000000-0000-4000-8000-000000000012")
	closedClaim := claimWorkflowDelivery(t, database, ctx, closeDelivery)
	closed, err := database.CompleteWebhookTransition(ctx, closedClaim.DeliveryID, closedClaim.ClaimToken, normalizedPayload(closedClaim.DeliveryID, "closed"), workflowLocator(), func(c store.WorkflowEventContext) (workflow.Event, error) {
		return workflow.IssueClosedEvent{EventMetadata: c.Metadata, ClosureID: "admission-closure", RetainUntil: c.Metadata.ObservedAt.Add(time.Hour), RetentionToken: "admission-retention"}, nil
	})
	if err != nil || closed.State != workflow.StateClosing {
		t.Fatalf("closure: %+v %v", closed, err)
	}
	var status string
	if err = pool.QueryRow(ctx, `SELECT status FROM review_admission_requests WHERE id=$1`, request.ID).Scan(&status); err != nil || status != "CANCELLED" {
		t.Fatalf("closure did not invalidate wait: %s %v", status, err)
	}
	if _, err = database.RecordReviewAdmission(ctx, lease, request.ID, store.ReviewAdmissionObservation{Generation: replacement.Generation, HeadSHA: replacement.HeadSHA, Release: true, Evidence: json.RawMessage(`{"release_reason":"completed_ci"}`)}); err == nil {
		t.Fatal("CI completion resurrected closed automation")
	}
}

func TestReviewGateTimeoutAndImmediateUseActualPromptContext(t *testing.T) {
	for _, immediate := range []bool{false, true} {
		t.Run(fmt.Sprint(immediate), func(t *testing.T) {
			database, pool, ctx, lease := reviewAdmissionFixture(t)
			provider := &admissionProvider{head: strings.Repeat("a", 40), base: strings.Repeat("b", 40), merge: strings.Repeat("c", 40), pending: true}
			server := httptest.NewServer(provider)
			defer server.Close()
			client, err := githubapi.NewAPIClient(server.Client(), server.URL)
			if err != nil {
				t.Fatal(err)
			}
			source := admissionPolicySource{workflowQualificationProfiles: workflowQualificationProfiles{head: strings.Repeat("d", 40)}, immediate: immediate}
			gate := agentturn.ReviewGate{Store: database, Source: source, Observer: client}
			request, err := database.BeginReviewAdmission(ctx, lease)
			if err != nil {
				t.Fatal(err)
			}
			now := request.CreatedAt.Add(31 * time.Minute)
			if immediate {
				now = request.CreatedAt
			}
			gate.Now = func() time.Time { return now }
			request, err = gate.Check(ctx, agentturn.Request{Lease: lease, InstallationCredential: "fixture", RepositoryOwner: "owner", RepositoryName: "repo"}, request)
			if err != nil {
				t.Fatal(err)
			}
			if request.Status != "RELEASED" {
				t.Fatalf("release path waited: %+v", request)
			}
			reason := "timeout_incomplete"
			if immediate {
				reason = "immediate"
				if provider.discoveryCount() != 0 {
					t.Fatal("immediate discovered CI")
				}
			}
			spec := preparationSpec("profile", "provider/model")
			spec.ReviewAdmission = &request
			prepared, err := database.PrepareAgentTurn(ctx, lease, spec)
			if err != nil {
				t.Fatal(err)
			}
			turnLease := acquireAndBindTurn(t, database, pool, ctx, prepared, "review-context")
			execution, err := database.GetAgentTurnExecutionContext(ctx, turnLease)
			if err != nil {
				t.Fatal(err)
			}
			config := builtinStoreConfig(t)
			envelope, err := agentturn.BuildEventEnvelope(execution, request.HeadSHA, config.Reducer.Definition(), config.Policies)
			if err != nil {
				t.Fatal(err)
			}
			var actual struct {
				ReviewStart struct {
					Reason string `json:"release_reason"`
					Head   string `json:"reviewed_head"`
				} `json:"review_start"`
			}
			if json.Unmarshal([]byte(envelope), &actual) != nil || actual.ReviewStart.Reason != reason || actual.ReviewStart.Head != request.HeadSHA {
				t.Fatalf("actual prompt context = %s", envelope)
			}
		})
	}
}

type admissionPolicySource struct {
	workflowQualificationProfiles
	immediate bool
}

func (s admissionPolicySource) ListRepositoryDirectoryFiles(ctx context.Context, token, owner, repo, dir, sha string) ([]string, error) {
	if dir == ".omnigrex" && s.immediate {
		return []string{".omnigrex/review-policy.yaml"}, nil
	}
	return s.workflowQualificationProfiles.ListRepositoryDirectoryFiles(ctx, token, owner, repo, dir, sha)
}
func (s admissionPolicySource) FetchRepositoryFile(ctx context.Context, token, owner, repo, path, sha string) ([]byte, error) {
	if path == ".omnigrex/review-policy.yaml" {
		return []byte("version: 1\nreview:\n  start: immediate\n"), nil
	}
	return s.workflowQualificationProfiles.FetchRepositoryFile(ctx, token, owner, repo, path, sha)
}

func TestReviewAdmissionRetryReusesSnapshotAndDeadline(t *testing.T) {
	database, pool, ctx, lease := reviewAdmissionFixture(t)
	root, err := prepareWithFixtureAdmission(t, database, ctx, lease, preparationSpec("profile", "provider/model"))
	if err != nil {
		t.Fatal(err)
	}
	var requestID string
	var originalDeadline time.Time
	var originalPolicy []byte
	if err = pool.QueryRow(ctx, `SELECT request.id::text,deadline,policy FROM review_admission_requests request JOIN review_admission_jobs link ON link.request_id=request.id WHERE link.job_id=$1`, lease.ID).Scan(&requestID, &originalDeadline, &originalPolicy); err != nil {
		t.Fatal(err)
	}
	running := acquireAndBindTurn(t, database, pool, ctx, root, "retry-admission")
	settleAcquiredTurn(t, database, ctx, running, store.AgentTurnFailed)
	setWorkflowRevision(t, pool, lease.WorkflowID, 2)
	insertPreparationJob(t, pool, lease.WorkflowID, lease.WorkflowAttemptID, 2, workflow.AssignmentGenerationCurrent, workflow.RoleReviewer, workflow.TurnPurposeRetry, root.Turn.ExpectedHeadSHA, root.Turn.ID)
	retryLease := claimPreparationJob(t, database, ctx)
	request, err := database.BeginReviewAdmission(ctx, retryLease)
	if err != nil {
		t.Fatal(err)
	}
	if request.ID != requestID || !request.Deadline.Equal(originalDeadline) || string(request.Policy) != string(originalPolicy) || request.Status != "WAITING" || len(request.ReleaseEvidence) > 0 {
		t.Fatalf("retry reset or blindly reused evidence: %+v", request)
	}
	if _, err = database.PrepareAgentTurn(ctx, retryLease, preparationSpec("profile", "provider/model")); !errors.Is(err, store.ErrReviewAdmissionPending) {
		t.Fatalf("retry bypassed fresh admission: %v", err)
	}
	// An expired original request can release with incomplete evidence; it cannot
	// create a second waiting interval anchored at retry time.
	request, err = database.RecordReviewAdmission(ctx, retryLease, request.ID, store.ReviewAdmissionObservation{Generation: request.Generation, HeadSHA: request.HeadSHA, Evidence: json.RawMessage(`{"release_reason":"timeout_incomplete"}`), Release: true})
	if err != nil {
		t.Fatal(err)
	}
	spec := preparationSpec("profile", "provider/model")
	spec.ReviewAdmission = &request
	if _, err = database.PrepareAgentTurn(ctx, retryLease, spec); err != nil {
		t.Fatal(err)
	}
}

func TestReviewPresentationObservationFenceRepairsSameRevisionRace(t *testing.T) {
	database, pool, ctx, lease := reviewAdmissionFixture(t)
	request, err := database.BeginReviewAdmission(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	request, err = database.RecordReviewAdmission(ctx, lease, request.ID, store.ReviewAdmissionObservation{Generation: request.Generation, HeadSHA: request.HeadSHA, Evidence: json.RawMessage(`{"release_reason":"waiting"}`)})
	if err != nil {
		t.Fatal(err)
	}
	effectLease, err := database.ClaimWorkflowGitHubEffectJob(ctx, store.ReconcileGitHubLabelsJobKind, "effect", time.Minute)
	if err != nil || effectLease == nil {
		t.Fatalf("effect claim: %v", err)
	}
	old, err := database.GetWorkflowGitHubEffectContext(ctx, *effectLease)
	if err != nil {
		t.Fatal(err)
	}
	request, err = database.RecordReviewAdmission(ctx, lease, request.ID, store.ReviewAdmissionObservation{Generation: request.Generation, HeadSHA: request.HeadSHA, Evidence: json.RawMessage(`{"release_reason":"completed_ci"}`), Release: true})
	if err != nil {
		t.Fatal(err)
	}
	acknowledgement, err := database.AcknowledgeWorkflowGitHubEffect(ctx, *effectLease, old, json.RawMessage(`{}`))
	if err != nil || !acknowledgement.Superseded {
		t.Fatalf("same-revision stale waiting effect not superseded: %+v %v", acknowledgement, err)
	}
	var repairs int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE workflow_id=$1 AND kind='RECONCILE_GITHUB_LABELS' AND status='AVAILABLE'`, lease.WorkflowID).Scan(&repairs); err != nil || repairs == 0 {
		t.Fatalf("stale presentation left no durable repair: %d %v", repairs, err)
	}
}

func (p *admissionProvider) discoveryCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.discoveries
}

func TestReviewGateRetriesUnavailableDiscoveryThenFreezesInventory(t *testing.T) {
	database, _, ctx, lease := reviewAdmissionFixture(t)
	provider := &admissionProvider{head: strings.Repeat("a", 40), base: strings.Repeat("b", 40), merge: strings.Repeat("c", 40), pending: true, discoveryUnavailable: true}
	server := httptest.NewServer(provider)
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	gate := agentturn.ReviewGate{Store: database, Source: workflowQualificationProfiles{head: strings.Repeat("d", 40)}, Observer: client}
	request, err := database.BeginReviewAdmission(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	deadline := request.Deadline
	check := func() {
		t.Helper()
		request, err = gate.Check(ctx, agentturn.Request{Lease: lease, InstallationCredential: "fixture", RepositoryOwner: "owner", RepositoryName: "repo"}, request)
		if err != nil {
			t.Fatal(err)
		}
	}
	check()
	if request.Status != "WAITING" || len(request.Policy) == 0 || len(request.Inventory) > 0 || !strings.Contains(string(request.Observation), "insufficient_permissions") {
		t.Fatalf("failed discovery became empty frozen policy: %+v", request)
	}
	provider.mu.Lock()
	provider.discoveryUnavailable = false
	provider.mu.Unlock()
	check()
	if request.Status != "WAITING" || len(request.Inventory) == 0 || !strings.Contains(string(request.Inventory), "build") || !request.Deadline.Equal(deadline) {
		t.Fatalf("discovery recovery lost request anchor: %+v", request)
	}
	provider.mu.Lock()
	provider.discoveryUnavailable = true
	provider.pending = false
	provider.mu.Unlock()
	check()
	if request.Status != "RELEASED" || provider.discoveryCount() != 2 {
		t.Fatalf("complete snapshot was not frozen: %+v discoveries=%d", request, provider.discoveryCount())
	}
}
