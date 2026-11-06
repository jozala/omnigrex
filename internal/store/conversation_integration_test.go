//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestConversationAssessmentFencesAllRoleTerminalAdmissionAndRecoversExactly(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for i, role := range []workflow.Role{workflow.RolePlanner, workflow.RoleDeveloper, workflow.RoleReviewer} {
		t.Run(string(role), func(t *testing.T) {
			fixture, lease, _ := prepareOpenSettlementTurn(t, databases[0], pool, ctx, 1900+i, role, strings.Repeat("a", 40))
			state := collaboration.State{}
			admitted := state.Accept("internal-input", collaboration.Command{Kind: "steer", Body: "Reconsider the historical retention decision."}, collaboration.Admission{Active: true, Authorized: true, ParticipantID: lease.AgentParticipantID, Actor: collaboration.Actor{ID: "human"}, Source: collaboration.Source{Channel: "fixture", ID: "source"}, At: time.Now()})
			if admitted.ResponseID == "" {
				t.Fatal(admitted)
			}
			state.Prepare(lease.AgentParticipantID, lease.ID)
			raw, _ := json.Marshal(state)
			if _, err := pool.Exec(ctx, `INSERT INTO work_item_collaboration(workflow_id,state) VALUES($1,$2)`, fixture.workflowID, raw); err != nil {
				t.Fatal(err)
			}
			tool := "request_review"
			if role == workflow.RoleReviewer {
				tool = "submit_review"
			}
			if role == workflow.RolePlanner {
				tool = "complete_planning"
			}
			terminal := store.MutationSpec{OperationID: "terminal", ToolName: tool, Request: json.RawMessage(`{"operation_id":"terminal"}`), ExternalService: "omnigrex", ExternalResourceID: fixture.workflowID}
			if _, err := databases[0].ReserveMutation(ctx, lease, terminal); !errors.Is(err, store.ErrUnresolvedClarification) {
				t.Fatal("delivery bypassed assessment", err)
			}
			apply := func(label, disposition string) string {
				t.Helper()
				batch := []collaboration.Assessment{{InputID: "internal-input", EvidenceIDs: []string{"internal-input"}, Corrects: []string{}, Disposition: disposition, Explanation: "The agent explicitly records material uncertainty or its evidence-based resolution."}}
				request, _ := json.Marshal(map[string]any{"operation_id": label, "assessments": []map[string]any{{"input_id": batch[0].InputID, "evidence_ids": batch[0].EvidenceIDs, "corrects": []string{}, "disposition": disposition, "explanation": batch[0].Explanation}}})
				m, err := databases[0].ReserveMutation(ctx, lease, store.MutationSpec{OperationID: label, ToolName: "assess_human_input", Request: request, ExternalService: "omnigrex", ExternalResourceID: fixture.workflowID})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = databases[0].StartMutation(ctx, lease, m.ID); err != nil {
					t.Fatal(err)
				}
				if err = databases[0].AssessHumanInput(ctx, lease, m.ID, batch); err != nil {
					t.Fatal(err)
				}
				// A new Store observation after a lost acknowledgment proves this exact
				// reservation's effect, not merely equal content from another operation.
				found, err := databases[1].HasHumanInputAssessmentEffect(ctx, fixture.workflowID, m.ID, lease.AgentParticipantID, batch)
				if err != nil || !found {
					t.Fatal(found, err)
				}
				found, err = databases[1].HasHumanInputAssessmentEffect(ctx, fixture.workflowID, "another-operation", lease.AgentParticipantID, batch)
				if err != nil || found {
					t.Fatal("unattributed assessment accepted", found, err)
				}
				if err = databases[0].AssessHumanInput(ctx, lease, m.ID, batch); err != nil {
					t.Fatal("same operation was not idempotent", err)
				}
				if err = databases[0].CompleteMutation(ctx, lease, m.ID, json.RawMessage(`{"recorded":true}`)); err != nil {
					t.Fatal(err)
				}
				return m.ID
			}
			apply("uncertainty", "uncertain")
			if _, err := databases[0].ReserveMutation(ctx, lease, terminal); !errors.Is(err, store.ErrUnresolvedClarification) {
				t.Fatal("historical uncertainty bypassed terminal fence", err)
			}
			apply("reconciled", "unchanged")
			if _, err := databases[0].ReserveMutation(ctx, lease, terminal); err != nil {
				t.Fatal("explicitly reconciled input remained fenced", err)
			}
			if err := pool.QueryRow(ctx, `SELECT state FROM work_item_collaboration WHERE workflow_id=$1`, fixture.workflowID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			if len(state.Assessments) != 2 || state.Inputs[0].Body != "Reconsider the historical retention decision." {
				t.Fatal("assessment replay rewrote source or duplicated effect")
			}
		})
	}
}

func TestConcurrentInputReferencesAndReceiptPublicationReservation(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fixture, _, _ := prepareOpenSettlementTurn(t, databases[0], pool, ctx, 1910, workflow.RoleDeveloper, "")
	submission := func(source string) store.HumanInputSubmission {
		return store.HumanInputSubmission{WorkflowID: fixture.workflowID, Command: collaboration.Command{Kind: "steer", Body: "Keep the agreed scope."}, Actor: collaboration.Actor{ID: "human"}, Source: collaboration.Source{Channel: "fixture", ID: source}, Authorized: true, At: time.Now()}
	}
	const count = 8
	receipts := make(chan collaboration.Receipt, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := databases[i%2].AdmitHumanInput(ctx, submission(fmt.Sprint(i)))
			receipts <- r
			errs <- err
		}(i)
	}
	wg.Wait()
	close(receipts)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var receipt collaboration.Receipt
	for r := range receipts {
		if r.ResponseID == "" {
			t.Fatal(r)
		}
		receipt = r
	}
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT state FROM work_item_collaboration WHERE workflow_id=$1`, fixture.workflowID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var state collaboration.State
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, in := range state.Inputs {
		if seen[in.Reference] || in.Reference == "" {
			t.Fatal("reference rebound", in)
		}
		seen[in.Reference] = true
	}
	for i := 1; i <= count; i++ {
		if !seen[fmt.Sprintf("A%d", i)] {
			t.Fatal("non-durable reference sequence", seen)
		}
	}
	// Source deduplication preserves the exact reference despite new UUID allocation.
	duplicate, err := databases[1].AdmitHumanInput(ctx, submission(receipt.Source.ID))
	if err != nil || duplicate.ID != receipt.ID || duplicate.ResponseID != receipt.ResponseID {
		t.Fatal(duplicate, err)
	}
	// Independent receipt publication ownership is serialized too. Only one
	// contender can POST; the rest must reconcile its exact frozen payload.
	acquired := make(chan bool, count)
	errs = make(chan error, count)
	artifact := collaboration.RenderedPublication{Body: receipt.Feedback + "\n\nfixture-marker", Marker: "fixture-marker"}
	for i := range count {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			actual, owned, err := databases[i%2].ReserveCollaborationReceipt(ctx, fixture.workflowID, receipt.ID, artifact)
			if actual != artifact && err == nil {
				err = errors.New("receipt payload changed")
			}
			acquired <- owned != 0
			errs <- err
		}(i)
	}
	wg.Wait()
	close(acquired)
	close(errs)
	owners := 0
	for owned := range acquired {
		if owned {
			owners++
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if owners != 1 {
		t.Fatal("concurrent receipt POST authority", owners)
	}
	different := artifact
	different.Marker = "new-renderer-marker"
	actual, owned, err := databases[1].ReserveCollaborationReceipt(ctx, fixture.workflowID, receipt.ID, different)
	if err != nil || owned != 0 || actual != artifact {
		t.Fatal("restart regenerated receipt payload or granted repost", actual, owned, err)
	}

	// A proven rejection permits exactly one new owner, retaining frozen bytes.
	if err = databases[0].RejectCollaborationReceipt(ctx, fixture.workflowID, receipt.ID, 1); err != nil {
		t.Fatal(err)
	}
	actual, attempt, err := databases[1].ReserveCollaborationReceipt(ctx, fixture.workflowID, receipt.ID, different)
	if err != nil || attempt != 2 || actual != artifact {
		t.Fatal(actual, attempt, err)
	}
	if err = databases[0].RejectCollaborationReceipt(ctx, fixture.workflowID, receipt.ID, 1); !errors.Is(err, collaboration.ErrInvalid) {
		t.Fatal("stale rejection released new owner", err)
	}
	if _, attempt, err = databases[0].ReserveCollaborationReceipt(ctx, fixture.workflowID, receipt.ID, different); err != nil || attempt != 0 {
		t.Fatal("uncertain retry acquired second owner", attempt, err)
	}
}

// Keep legacy scenario fixtures on the new explicit-consideration contract;
// retrieval and question resolution alone must never count as assessment.
func assessDeliveredFixtureInputs(t *testing.T, database *store.Store, ctx context.Context, lease store.AgentTurnLease, label string, ids []string) {
	t.Helper()
	var batch []collaboration.Assessment
	for _, id := range ids {
		batch = append(batch, collaboration.Assessment{InputID: id, EvidenceIDs: []string{id}, Disposition: "changed", Explanation: "Explicit fixture evidence considered; the recorded interpretation governs."})
	}
	request, _ := json.Marshal(map[string]any{"operation_id": label, "assessments": batch})
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{ToolName: "assess_human_input", OperationID: label, Request: request, ExternalService: "omnigrex", ExternalResourceID: lease.JobLease.WorkflowID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	if err = database.AssessHumanInput(ctx, lease, mutation.ID, batch); err != nil {
		t.Fatal(err)
	}
	if err = database.CompleteMutation(ctx, lease, mutation.ID, json.RawMessage(`{"recorded":true}`)); err != nil {
		t.Fatal(err)
	}
}
