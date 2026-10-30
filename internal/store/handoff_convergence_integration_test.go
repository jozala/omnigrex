//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

// These fixtures use the real settlement, deferred-event replay, job fences,
// and publication/cleanup workers. Remote calls are an in-memory GitHub boundary.
func TestBlockedHandoffConvergesAcrossDeferredSynchronization(t *testing.T) {
	for _, boundary := range []string{"before publication", "two revisions before publication", "after Issue comment", "after PR comment"} {
		t.Run(boundary, func(t *testing.T) {
			databases, pool := openPhaseFiveStores(t, 1)
			database := databases[0]
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			fixture, lease, proposal := prepareSettlementTurn(t, database, pool, ctx, 950, workflow.RoleDeveloper, "head-a")
			if _, err := pool.Exec(ctx, `UPDATE workflows SET issue_number = 951 WHERE id = $1`, fixture.workflowID); err != nil {
				t.Fatal(err)
			}
			insertDeferredSettlementNormalizedEvent(t, pool, ctx, fixture.workflowID, lease.ID, deferredSettlementEvent{
				deliveryID: "78950000-0000-4000-8000-000000000001", eventName: "pull_request", action: "synchronize",
				beforeSHA: proposal.HeadSHA, headSHA: "head-b", createdAt: time.Now().UTC(),
			})
			if boundary == "two revisions before publication" {
				insertDeferredSettlementNormalizedEvent(t, pool, ctx, fixture.workflowID, lease.ID, deferredSettlementEvent{
					deliveryID: "78950000-0000-4000-8000-000000000003", eventName: "pull_request", action: "synchronize",
					beforeSHA: "head-b", headSHA: "head-c", createdAt: time.Now().UTC(),
				})
			}
			observation := successfulSettlementObservation(workflow.TurnOutcomeBlocked, nil)
			observation.Diagnostic = "distinctive current blocker: external qualification required"
			settled, err := database.SettleAgentTurn(ctx, lease, observation)
			if err != nil || settled.State != workflow.StateNeedsHuman || settled.ReconciliationJobID == "" {
				t.Fatalf("blocked settlement = (%#v, %v)", settled, err)
			}
			// A later safety diagnostic with the same reason and revision must
			// never become the carried normal explanation.
			safetyPayload, err := json.Marshal(map[string]any{
				"revision": settled.Revision, "reason": "agent_blocked", "diagnostic": "unrelated safety diagnostic", "safety_diagnostic": true,
			})
			if err != nil {
				t.Fatal(err)
			}
			safetyID := insertJob(t, pool, ctx, jobSeed{
				Queue: store.WorkflowActionQueue, Kind: store.PublishHumanHandoffJobKind, Payload: safetyPayload,
				MaxAttempts: 3, IdempotencyKey: "historical-safety", WorkflowID: fixture.workflowID,
			})
			if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'SUCCEEDED' WHERE id = $1`, safetyID); err != nil {
				t.Fatal(err)
			}
			replay := func() {
				job, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.ReconcilePendingEventsJobKind, "replay", 20*time.Second)
				if err != nil || job == nil {
					t.Fatalf("claim replay = (%#v, %v)", job, err)
				}
				ack, err := database.AcknowledgePendingEventReconciliation(ctx, *job, settlementPendingEventFactory)
				if err != nil || ack.SuccessorJobID != "" {
					t.Fatalf("replay = (%#v, %v)", ack, err)
				}
				var state, reason, head string
				var revision uint64
				if err := pool.QueryRow(ctx, `SELECT w.status, w.state_revision, w.human_handoff_reason, p.head_sha
FROM workflows w JOIN change_proposals p ON p.workflow_id = w.id AND p.active WHERE w.id = $1`, fixture.workflowID).Scan(&state, &revision, &reason, &head); err != nil {
					t.Fatal(err)
				}
				wantRevision, wantHead := settled.Revision+1, "head-b"
				if boundary == "two revisions before publication" {
					wantRevision, wantHead = settled.Revision+2, "head-c"
				}
				if state != "NEEDS_HUMAN" || revision != wantRevision || reason != "agent_blocked" || head != wantHead {
					t.Fatalf("replayed handoff = %s/%d/%s/%s", state, revision, reason, head)
				}
			}
			api := &handoffFixtureAPI{comments: map[int][]github.IssueComment{951: {{ID: 1, Body: "human Issue comment"}}, 950: {{ID: 2, Body: "human PR comment"}}}, nextID: 10}
			if boundary == "before publication" || boundary == "two revisions before publication" {
				replay()
			} else {
				api.afterCreate = func(number int) {
					if boundary == "after Issue comment" && number == 951 || boundary == "after PR comment" && number == 950 {
						api.afterCreate = nil
						replay()
					}
				}
			}
			worker, err := github.NewHumanHandoffWorker(database, handoffFixtureCredential{}, api, github.VisibleEffectWorkerConfig{
				ClaimOwner: "handoff", LeaseDuration: 20 * time.Second, HeartbeatInterval: 5 * time.Second, IdlePollInterval: time.Millisecond, RetryDelay: time.Millisecond,
			})
			if err != nil {
				t.Fatal(err)
			}
			for count := 0; ; count++ {
				processed, err := worker.ProcessNext(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if !processed {
					break
				}
				if count > 5 {
					t.Fatal("handoff jobs did not converge")
				}
			}
			for _, number := range []int{951, 950} {
				comments := api.comments[number]
				if len(comments) != 2 || !strings.HasPrefix(comments[0].Body, "human ") || !strings.Contains(comments[1].Body, observation.Diagnostic) {
					t.Errorf("artifact %d comments = %#v, want human comment and current explanation", number, comments)
				}
			}
			var pending int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind = 'PUBLISH_HUMAN_HANDOFF' AND status <> 'SUCCEEDED'`, fixture.workflowID).Scan(&pending); err != nil {
				t.Fatal(err)
			}
			if pending != 0 {
				t.Errorf("unfinished handoff jobs = %d", pending)
			}
		})
	}
}

type handoffFixtureCredential struct{}

func (handoffFixtureCredential) RepositoryCredential(context.Context, string, string) (string, error) {
	return "fixture-token", nil
}

type handoffFixtureAPI struct {
	comments    map[int][]github.IssueComment
	nextID      int64
	afterCreate func(int)
}

func (api *handoffFixtureAPI) ListIssueComments(_ context.Context, _, _, _ string, number int) ([]github.IssueComment, error) {
	return append([]github.IssueComment(nil), api.comments[number]...), nil
}
func (api *handoffFixtureAPI) CreateIssueComment(_ context.Context, _, _, _ string, number int, request github.CommentRequest) (github.IssueComment, error) {
	return api.create(number, request)
}
func (api *handoffFixtureAPI) CreatePullRequestComment(_ context.Context, _, _, _ string, number int, request github.CommentRequest) (github.IssueComment, error) {
	return api.create(number, request)
}
func (api *handoffFixtureAPI) create(number int, request github.CommentRequest) (github.IssueComment, error) {
	api.nextID++
	comment := github.IssueComment{ID: api.nextID, Body: request.Body + "\n\n" + request.Marker, HTMLURL: fmt.Sprintf("https://example.test/comment/%d", api.nextID)}
	api.comments[number] = append(api.comments[number], comment)
	if api.afterCreate != nil {
		api.afterCreate(number)
	}
	// Exercise ambiguous success: the worker must observe the marker, not repost.
	return github.IssueComment{}, errors.New("remote response lost after creation")
}
func (api *handoffFixtureAPI) DeleteIssueComment(_ context.Context, _, _, _ string, id int64) error {
	for number, comments := range api.comments {
		for index, comment := range comments {
			if comment.ID == id {
				api.comments[number] = append(comments[:index], comments[index+1:]...)
				return nil
			}
		}
	}
	return nil
}
