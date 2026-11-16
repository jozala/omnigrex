package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/reviewpolicy"
	"github.com/jozala/omnigrex/internal/workflow"
)

type ReviewDisposition string

const (
	ReviewWait     ReviewDisposition = "wait"
	ReviewAdmit    ReviewDisposition = "admit"
	ReviewRepair   ReviewDisposition = "repair"
	ReviewEscalate ReviewDisposition = "handoff"
)

var ErrReviewAdmissionRouted = errors.New("review admission routed without a Reviewer")
var ErrReviewAdmissionPending = errors.New("Reviewer CI admission pending or target replaced")

type ReviewAdmission struct {
	AcceptedPlanRevisionID                          string
	PullRequestID, PullRequestNumber, RepositoryID  int64
	ID                                              string
	Generation                                      int64
	HeadSHA, BaseSHA, TestMergeSHA, PolicySourceSHA string
	CreatedAt, Deadline, GraceAnchor                time.Time
	Policy, Inventory, Observation, ReleaseEvidence json.RawMessage
	RepairUsage                                     int
	RepairTurnAllocated                             bool
	Status                                          string
}
type ReviewAdmissionObservation struct {
	Generation                     int64
	HeadSHA, BaseSHA, TestMergeSHA string
	PolicySourceSHA                string
	Policy, Inventory, Evidence    json.RawMessage
	Release                        bool
	Disposition                    ReviewDisposition
}

const admissionSelect = `SELECT id::text, pull_request_id, pull_request_number, repository_id, generation, head_sha, base_sha, test_merge_sha, policy_source_sha,
 created_at, deadline, grace_anchor, policy, inventory, last_observation, release_evidence, status, COALESCE(accepted_plan_revision_id,'')
 FROM review_admission_requests `

func scanAdmission(row pgx.Row) (r ReviewAdmission, err error) {
	err = row.Scan(&r.ID, &r.PullRequestID, &r.PullRequestNumber, &r.RepositoryID, &r.Generation, &r.HeadSHA, &r.BaseSHA, &r.TestMergeSHA, &r.PolicySourceSHA, &r.CreatedAt, &r.Deadline, &r.GraceAnchor, &r.Policy, &r.Inventory, &r.Observation, &r.ReleaseEvidence, &r.Status, &r.AcceptedPlanRevisionID)
	return
}

// BeginReviewAdmission anchors the wait before any external configuration or CI reads.
// Retry jobs link back through the failed Turn, retaining the original request/deadline.
func (s *Store) BeginReviewAdmission(ctx context.Context, lease JobLease) (ReviewAdmission, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ReviewAdmission{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	job, payload, err := s.lockReviewAdmissionJob(ctx, tx, lease)
	if err != nil {
		return ReviewAdmission{}, err
	}
	if _, err = s.lockPreparationWorkflow(ctx, tx, job, payload); err != nil {
		return ReviewAdmission{}, err
	}
	r, err := scanAdmission(tx.QueryRow(ctx, admissionSelect+` WHERE id = (SELECT request_id FROM review_admission_jobs WHERE job_id=$1) FOR UPDATE`, job.ID))
	if errors.Is(err, pgx.ErrNoRows) && payload.RetryOfTurnID != "" {
		r, err = scanAdmission(tx.QueryRow(ctx, admissionSelect+` WHERE id=(SELECT link.request_id FROM agent_turns turn JOIN review_admission_jobs link ON link.job_id=turn.preparation_job_id WHERE turn.id=$1) AND workflow_id=$2 AND workflow_attempt_id=$3 FOR UPDATE`, payload.RetryOfTurnID, job.WorkflowID, job.WorkflowAttemptID))
		if err == nil {
			// A retry is a fresh point-in-time observation, not reuse of an old release.
			_, err = tx.Exec(ctx, `UPDATE review_admission_requests SET status='WAITING', release_evidence=NULL, admitted_turn_id=NULL, updated_at=clock_timestamp() WHERE id=$1`, r.ID)
			r.Status = "WAITING"
			r.ReleaseEvidence = nil
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		r, err = scanAdmission(tx.QueryRow(ctx, `INSERT INTO review_admission_requests(workflow_id,workflow_attempt_id,stage_id,source_job_id,created_at,deadline,grace_anchor,head_sha,pull_request_id,pull_request_number,repository_id,accepted_plan_revision_id)
  SELECT $1,$2,$3,$4,$5,$5::timestamptz + interval '2 hours',$5,$6,pull_request_id,pull_request_number,repository_id,(SELECT state->'planning'->'implementation'->>'accepted_revision_id' FROM work_item_collaboration WHERE workflow_id=$1) FROM change_proposals WHERE workflow_id=$1 AND active
  RETURNING id::text,pull_request_id,pull_request_number,repository_id,generation,head_sha,base_sha,test_merge_sha,policy_source_sha,created_at,deadline,grace_anchor,policy,inventory,last_observation,release_evidence,status,COALESCE(accepted_plan_revision_id,'')`, job.WorkflowID, job.WorkflowAttemptID, payload.Stage, job.ID, job.CreatedAt, payload.ExpectedHeadSHA))
	}
	if err != nil {
		return ReviewAdmission{}, fmt.Errorf("anchor review admission: %w", err)
	}
	if err := validateReviewPlanTx(ctx, tx, job.WorkflowID, r.AcceptedPlanRevisionID); err != nil {
		return r, err
	}
	if r.Status != "WAITING" && r.Status != "RELEASED" {
		return ReviewAdmission{}, ErrAgentTurnPreparationFenceLost
	}
	if _, err = tx.Exec(ctx, `INSERT INTO review_admission_jobs(job_id,request_id) VALUES ($1,$2) ON CONFLICT (job_id) DO NOTHING`, job.ID, r.ID); err != nil {
		return ReviewAdmission{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ReviewAdmission{}, err
	}
	return r, nil
}
func (s *Store) lockReviewAdmissionJob(ctx context.Context, tx pgx.Tx, lease JobLease) (Job, agentTurnPreparationPayload, error) {
	if err := lockAgentTurnPreparationWorkflow(ctx, tx, lease.WorkflowID); err != nil {
		return Job{}, agentTurnPreparationPayload{}, err
	}
	job, err := lockPreparationJobTx(ctx, tx, lease)
	if err != nil {
		return job, agentTurnPreparationPayload{}, err
	}
	p, err := decodeAgentTurnPreparationPayload(job.Payload)
	if err != nil || !s.validAgentTurnPreparationPayload(p, job) || p.Role != workflow.RoleReviewer {
		return job, p, ErrAgentTurnPreparationFenceLost
	}
	var humanControlled bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_sessions WHERE workflow_id=$1 AND control_owner='HUMAN' AND status IN ('ACTIVE','RETAINED'))`, job.WorkflowID).Scan(&humanControlled); err != nil {
		return job, p, err
	}
	if humanControlled {
		return job, p, ErrAgentTurnPreparationFenceLost
	}
	return job, p, nil
}

// RecordReviewAdmission commits one bounded observation under the live job and target fence.
// Target replacement invalidates evidence and restarts grace, never the absolute deadline.
func (s *Store) RecordReviewAdmission(ctx context.Context, lease JobLease, id string, o ReviewAdmissionObservation) (ReviewAdmission, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ReviewAdmission{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	job, p, err := s.lockReviewAdmissionJob(ctx, tx, lease)
	if err != nil {
		return ReviewAdmission{}, err
	}
	if _, err = s.lockPreparationWorkflow(ctx, tx, job, p); err != nil {
		return ReviewAdmission{}, err
	}
	r, err := scanAdmission(tx.QueryRow(ctx, admissionSelect+` WHERE id=$1 AND id=(SELECT request_id FROM review_admission_jobs WHERE job_id=$2) FOR UPDATE`, id, job.ID))
	if err != nil {
		return r, err
	}
	if err := validateReviewPlanTx(ctx, tx, job.WorkflowID, r.AcceptedPlanRevisionID); err != nil {
		return r, err
	}
	if r.Generation != o.Generation || r.Status != "WAITING" && r.Status != "RELEASED" {
		return r, ErrReviewAdmissionPending
	}
	var currentPRID, currentPRNumber int64
	if err = tx.QueryRow(ctx, `SELECT pull_request_id,pull_request_number FROM change_proposals WHERE workflow_id=$1 AND active`, job.WorkflowID).Scan(&currentPRID, &currentPRNumber); err != nil {
		return r, err
	}
	if currentPRID != r.PullRequestID || currentPRNumber != r.PullRequestNumber {
		return r, ErrAgentTurnPreparationFenceLost
	}
	if o.HeadSHA != p.ExpectedHeadSHA { // Only a validated PR observation may retarget; caller supplies its verified identity.
		var prID, prNumber int64
		if err = tx.QueryRow(ctx, `SELECT pull_request_id,pull_request_number FROM change_proposals WHERE workflow_id=$1 AND active`, job.WorkflowID).Scan(&prID, &prNumber); err != nil {
			return r, err
		}
		if _, _, err = s.lockPreparationWorkflowVerified(ctx, tx, job, p, AgentTurnPreparationSpec{VerifiedHeadSHA: o.HeadSHA, VerifiedPRID: prID, VerifiedPRNumber: prNumber}); err != nil {
			return r, err
		}
		p.ExpectedHeadSHA = o.HeadSHA
		raw, _ := json.Marshal(p)
		if _, err = tx.Exec(ctx, `UPDATE jobs SET payload=$2 WHERE id=$1`, job.ID, raw); err != nil {
			return r, err
		}
	}
	previousPresentation := admissionPresentationKey(r.Observation)
	previousStatus := r.Status
	restartGrace := r.HeadSHA != o.HeadSHA || len(r.Observation) > 0
	targetChanged := r.HeadSHA != o.HeadSHA || r.BaseSHA != o.BaseSHA || r.TestMergeSHA != o.TestMergeSHA
	if !targetChanged {
		var previous, observed struct {
			At time.Time `json:"observed_at"`
		}
		_ = json.Unmarshal(r.Observation, &previous)
		_ = json.Unmarshal(o.Evidence, &observed)
		if !previous.At.IsZero() && !observed.At.IsZero() && observed.At.Before(previous.At) {
			return r, ErrReviewAdmissionPending
		}
	}
	if targetChanged {
		r.Generation++
		r.HeadSHA = o.HeadSHA
		r.BaseSHA = o.BaseSHA
		r.TestMergeSHA = o.TestMergeSHA
		if restartGrace {
			if err = tx.QueryRow(ctx, `SELECT LEAST(clock_timestamp(),$1::timestamptz)`, r.Deadline).Scan(&r.GraceAnchor); err != nil {
				return r, err
			}
		}
		// The supplied observation is for this replacement target; old generation
		// callbacks were rejected above. Grace/deadline evaluation uses the new anchor.
	}
	if r.PolicySourceSHA == "" && o.PolicySourceSHA != "" {
		r.PolicySourceSHA = o.PolicySourceSHA
	}
	if r.PolicySourceSHA != "" && o.PolicySourceSHA != "" && r.PolicySourceSHA != o.PolicySourceSHA {
		return r, ErrReviewAdmissionPending
	}
	if len(r.Policy) > 0 && len(o.Policy) > 0 {
		frozenPolicy, _ := canonicalJSON(r.Policy)
		observedPolicy, _ := canonicalJSON(o.Policy)
		if !bytes.Equal(frozenPolicy, observedPolicy) {
			return r, ErrReviewAdmissionPending
		}
	}
	if len(r.Inventory) > 0 && len(o.Policy) > 0 {
		frozenInventory, _ := canonicalJSON(r.Inventory)
		observedInventory, _ := canonicalJSON(o.Inventory)
		if !bytes.Equal(frozenInventory, observedInventory) {
			return r, ErrReviewAdmissionPending
		}
	}
	if len(r.Policy) == 0 && len(o.Policy) > 0 {
		var policy reviewpolicy.Policy
		if json.Unmarshal(o.Policy, &policy) != nil || policy.Validate() != nil {
			return r, reviewpolicy.ErrInvalid
		}
		r.Policy = o.Policy
		r.PolicySourceSHA = o.PolicySourceSHA
		if proposed := r.CreatedAt.Add(policy.Timeout); proposed.Before(r.Deadline) {
			r.Deadline = proposed
		}
	}
	if len(r.Inventory) == 0 && len(o.Inventory) > 0 {
		r.Inventory = o.Inventory
	}
	status := "WAITING"
	var release json.RawMessage
	if o.Release {
		status = "RELEASED"
		release = o.Evidence
	}
	_, err = tx.Exec(ctx, `UPDATE review_admission_requests SET generation=$2,head_sha=$3,base_sha=$4,test_merge_sha=$5,grace_anchor=$6,
 policy_source_sha=$7,policy=$8,inventory=$9,deadline=$10,last_observation=$11,release_evidence=$12,status=$13,updated_at=clock_timestamp() WHERE id=$1`, r.ID, r.Generation, r.HeadSHA, r.BaseSHA, r.TestMergeSHA, r.GraceAnchor, r.PolicySourceSHA, nullableJSON(r.Policy), nullableJSON(r.Inventory), r.Deadline, nullableJSON(o.Evidence), nullableJSON(release), status)
	if err != nil {
		return r, err
	}
	if previousStatus != status || targetChanged || !bytes.Equal(previousPresentation, admissionPresentationKey(o.Evidence)) {
		if err = enqueueReviewPresentationTx(ctx, tx, job.WorkflowID, job.WorkflowAttemptID); err != nil {
			return r, err
		}
	}
	r.Status = status
	r.Observation = o.Evidence
	r.ReleaseEvidence = release
	if o.Disposition == ReviewRepair || o.Disposition == ReviewEscalate {
		if err := s.routeReviewCITx(ctx, tx, job, r, o); err != nil {
			return r, err
		}
		r.Status = "ROUTED"
	}
	if err = tx.Commit(ctx); err != nil {
		return r, err
	}
	return r, nil
}

// DeferReviewAdmission releases the orchestrator lease as healthy delayed work.
// Increasing the remaining claim allowance preserves audit attempt numbers without
// charging healthy CI waits against the ordinary finite failure allowance.
func (s *Store) DeferReviewAdmission(ctx context.Context, lease JobLease, delay time.Duration) error {
	if delay < time.Second || delay > time.Minute {
		return errors.New("invalid review observation delay")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	job, p, err := s.lockReviewAdmissionJob(ctx, tx, lease)
	if err != nil {
		return err
	}
	if _, err = s.lockPreparationWorkflow(ctx, tx, job, p); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE job_attempts SET status='SUCCEEDED',finished_at=clock_timestamp(),result='{"healthy_ci_wait":true}' WHERE job_id=$1 AND lease_token=$2 AND attempt_number=$3 AND status='LEASED'`, job.ID, lease.LeaseToken, lease.Attempt)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE jobs SET status='AVAILABLE',max_attempts=max_attempts+1,available_at=clock_timestamp()+$2*interval '1 microsecond',lease_owner=NULL,lease_token=NULL,leased_at=NULL,lease_expires_at=NULL,heartbeat_at=NULL,updated_at=clock_timestamp() WHERE id=$1`, job.ID, delay.Microseconds())
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE review_admission_requests SET next_observation_at=clock_timestamp()+$2*interval '1 microsecond' WHERE id=(SELECT request_id FROM review_admission_jobs WHERE job_id=$1)`, job.ID, delay.Microseconds())
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func consumeReviewAdmissionTx(ctx context.Context, tx pgx.Tx, job Job, p agentTurnPreparationPayload, head string, fence *ReviewAdmission) (json.RawMessage, error) {
	if p.Role != workflow.RoleReviewer {
		return nil, nil
	}
	if fence == nil {
		return nil, ErrReviewAdmissionPending
	}
	r, err := scanAdmission(tx.QueryRow(ctx, admissionSelect+` WHERE id=$1 AND workflow_id=$2 AND workflow_attempt_id=$3 AND id=(SELECT request_id FROM review_admission_jobs WHERE job_id=$4) FOR UPDATE`, fence.ID, job.WorkflowID, job.WorkflowAttemptID, job.ID))
	if err != nil {
		return nil, fmt.Errorf("read admission fence: %w", err)
	}
	if err := validateReviewPlanTx(ctx, tx, job.WorkflowID, r.AcceptedPlanRevisionID); err != nil {
		return nil, err
	}
	if r.Status != "RELEASED" || r.Generation != fence.Generation || r.HeadSHA != head || len(r.ReleaseEvidence) == 0 {
		return nil, ErrReviewAdmissionPending
	}
	var sameProposal bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM change_proposals WHERE workflow_id=$1 AND active AND pull_request_id=$2 AND pull_request_number=$3 AND repository_id=$4)`, job.WorkflowID, r.PullRequestID, r.PullRequestNumber, r.RepositoryID).Scan(&sameProposal); err != nil || !sameProposal {
		return nil, ErrReviewAdmissionPending
	}
	var evidence struct {
		SemanticVersion int               `json:"semantic_version"`
		Disposition     ReviewDisposition `json:"disposition"`
		AllSuccessful   bool              `json:"all_successful"`
	}
	if json.Unmarshal(r.ReleaseEvidence, &evidence) != nil || evidence.SemanticVersion != 2 || evidence.Disposition != ReviewAdmit {
		return nil, ErrReviewAdmissionPending
	}
	if evidence.AllSuccessful {
		// Allocation and the reset intent share this transaction. A retry or
		// lost acknowledgement cannot replay this reset after a later repair.
		inserted, err := tx.Exec(ctx, `INSERT INTO review_ci_decisions(request_id,generation,workflow_attempt_id,decision,workflow_revision,evidence)
 SELECT $1,$2,$3,'reset',state_revision,$4 FROM workflows WHERE id=$5 ON CONFLICT(request_id,decision) DO NOTHING`, r.ID, r.Generation, job.WorkflowAttemptID, r.ReleaseEvidence, job.WorkflowID)
		if err != nil {
			return nil, err
		}
		if inserted.RowsAffected() == 1 {
			if _, err := tx.Exec(ctx, `UPDATE workflow_attempts SET ci_repair_usage=0 WHERE id=$1 AND active`, job.WorkflowAttemptID); err != nil {
				return nil, err
			}
		}
	}
	// The Turn FK is assigned after allocation in the same transaction.
	return r.ReleaseEvidence, nil
}

// Observation clocks are not meaningful presentation changes; result identities,
// target generations, failures and release reasons are.
func admissionPresentationKey(raw json.RawMessage) []byte {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	var strip func(any)
	strip = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			delete(x, "observed_at")
			for _, child := range x {
				strip(child)
			}
		case []any:
			for _, child := range x {
				strip(child)
			}
		}
	}
	strip(value)
	key, _ := json.Marshal(value)
	return key
}
func enqueueReviewPresentationTx(ctx context.Context, tx pgx.Tx, workflowID, attemptID string) error {
	var version, revision int64
	if err := tx.QueryRow(ctx, `UPDATE workflows SET review_presentation_revision=review_presentation_revision+1 WHERE id=$1 RETURNING review_presentation_revision,state_revision`, workflowID).Scan(&version, &revision); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"revision": revision})
	_, err := insertJobTx(ctx, tx, jobInsert{queue: WorkflowActionQueue, kind: ReconcileGitHubLabelsJobKind, payload: payload, maxAttempts: 3, idempotencyKey: fmt.Sprintf("review-presentation:%s:%d", workflowID, version), scope: jobInsertScope{workflowID: workflowID, workflowAttemptID: attemptID}})
	return err
}

func cancelReviewAdmissionForHumanControlTx(ctx context.Context, tx pgx.Tx, workflowID string) error {
	var attemptID string
	err := tx.QueryRow(ctx, `UPDATE review_admission_requests SET status='CANCELLED',release_evidence=NULL,updated_at=clock_timestamp() WHERE workflow_id=$1 AND status IN ('WAITING','RELEASED') RETURNING workflow_attempt_id::text`, workflowID).Scan(&attemptID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return enqueueReviewPresentationTx(ctx, tx, workflowID, attemptID)
}

// Only the CI-gated Reviewer preparation permits a coalesced head/revision in
// its payload. All immutable intent and ordinary lease/attempt fences remain
// checked by the shared lock; no other job kind gets a weaker payload fence.
func lockPreparationJobTx(ctx context.Context, tx pgx.Tx, lease JobLease) (Job, error) {
	var current json.RawMessage
	if err := tx.QueryRow(ctx, `SELECT payload FROM jobs WHERE id=$1 FOR UPDATE`, lease.ID).Scan(&current); err != nil {
		return Job{}, ErrAgentTurnPreparationFenceLost
	}
	current, err := canonicalJSON(current)
	if err != nil {
		return Job{}, ErrAgentTurnPreparationFenceLost
	}
	if !bytes.Equal(current, lease.Payload) {
		old, err := decodeAgentTurnPreparationPayload(lease.Payload)
		if err != nil {
			return Job{}, ErrAgentTurnPreparationFenceLost
		}
		updated, err := decodeAgentTurnPreparationPayload(current)
		if err != nil {
			return Job{}, ErrAgentTurnPreparationFenceLost
		}
		old.ExpectedHeadSHA = updated.ExpectedHeadSHA
		old.Revision = updated.Revision
		if old != updated || updated.Role != workflow.RoleReviewer {
			return Job{}, ErrAgentTurnPreparationFenceLost
		}
		lease.Payload = current
	}
	return lockFencedWorkflowJob(ctx, tx, lease, PrepareAgentTurnJobKind, ErrAgentTurnPreparationFenceLost)
}

func validateReviewPlanTx(ctx context.Context, tx pgx.Tx, workflowID, plan string) error {
	state, err := readCollaborationTx(ctx, tx, workflowID)
	if err != nil {
		return err
	}
	if state.ActiveReplanning() != nil {
		return ErrReviewAdmissionPending
	}
	current := ""
	if state.Planning != nil {
		current = state.Planning.Implementation.AcceptedRevisionID
	}
	if current != plan {
		return ErrReviewAdmissionPending
	}
	return nil
}

// routeReviewCITx retires preparation and dispatches at most one successor under
// the same Workflow, request-generation and live-lease locks as admission.
func (s *Store) routeReviewCITx(ctx context.Context, tx pgx.Tx, job Job, r ReviewAdmission, o ReviewAdmissionObservation) error {
	snapshot, err := rehydrateWorkflow(ctx, tx, job.WorkflowID)
	if err != nil {
		return err
	}
	diagnostic := fmt.Sprintf("CI admission %s for PR #%d head %s; repair usage %d/3; deadline %s. Inspect scoped CI tools for selected results and diagnostics.", o.Disposition, r.PullRequestNumber, r.HeadSHA, snapshot.CurrentAttempt.CIRepairUsage, r.Deadline.UTC().Format(time.RFC3339))
	diagnostic += admissionFailureSummary(o.Evidence)
	decision := s.reducer.Reduce(snapshot, workflow.CIAdmissionEvent{
		EventMetadata: workflow.EventMetadata{ID: r.ID, ObservedAt: time.Now().UTC(), WorkItem: snapshot.WorkItem, ExpectedRevision: snapshot.Revision},
		AttemptID:     job.WorkflowAttemptID, ChangeProposalID: snapshot.ChangeProposal.ID, HeadSHA: r.HeadSHA, Repair: o.Disposition == ReviewRepair, Diagnostic: diagnostic,
	})
	if err := validateWorkflowDecision(snapshot, decision); err != nil {
		return err
	}
	if decision.Disposition != workflow.DispositionApplied {
		return ErrAgentTurnPreparationFenceLost
	}
	kind := o.Disposition
	if decision.Snapshot.State == workflow.StateNeedsHuman {
		kind = ReviewEscalate
	}
	if _, err := tx.Exec(ctx, `INSERT INTO review_ci_decisions(request_id,generation,workflow_attempt_id,decision,workflow_revision,evidence) VALUES($1,$2,$3,$4,$5,$6)`, r.ID, r.Generation, job.WorkflowAttemptID, kind, decision.Snapshot.Revision, o.Evidence); err != nil {
		return err
	}
	delivery, settlement, err := workflowJobActionProvenance(ctx, tx, job.ID)
	if err != nil {
		return err
	}
	if err := completeAcknowledgementJobTx(ctx, tx, job, o.Evidence, ErrAgentTurnPreparationFenceLost); err != nil {
		return err
	}
	if err := persistAppliedDecisionWithProvenance(ctx, tx, delivery, settlement, job.WorkflowID, snapshot, decision, "ci-admission:"+r.ID); err != nil {
		return err
	}
	// persistWorkflowSnapshot cancels the retired request as the Stage changes.
	return nil
}

func admissionFailureSummary(raw json.RawMessage) string {
	var observation struct {
		Targets []struct {
			Results []struct {
				Requirement struct {
					Context string `json:"context"`
				} `json:"requirement"`
				Source     string `json:"source"`
				ID         int64  `json:"result_id"`
				Conclusion string `json:"conclusion"`
				State      string `json:"state"`
			} `json:"results"`
		} `json:"targets"`
	}
	if json.Unmarshal(raw, &observation) != nil {
		return ""
	}
	var out strings.Builder
	count := 0
	for _, target := range observation.Targets {
		for _, r := range target.Results {
			if r.State != "terminal_non_success" || r.Conclusion == "neutral" || r.Conclusion == "skipped" || count >= 8 {
				continue
			}
			clean := func(value string) string {
				value = strings.Map(func(r rune) rune {
					if r < ' ' || strings.ContainsRune("`<>@[]", r) {
						return ' '
					}
					return r
				}, value)
				runes := []rune(value)
				if len(runes) > 120 {
					runes = runes[:120]
				}
				return string(runes)
			}
			fmt.Fprintf(&out, " Selected %s %s #%d: %s.", clean(r.Requirement.Context), clean(r.Source), r.ID, clean(r.Conclusion))
			count++
		}
	}
	return out.String()
}
