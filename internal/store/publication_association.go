package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/publication"
	"github.com/jozala/omnigrex/internal/workflow"
)

// settlementPublication is derived inside the caller's settlement transaction,
// not supplied by the Agent or accepted as readiness. Its invocation IDs and
// post-open edges are retained in settlement result JSON for replay validation.
type settlementPublication struct {
	Proposal             AgentTurnSettlementChangeProposal `json:"proposal"`
	OpenInvocationID     string                            `json:"open_invocation_id"`
	PublishInvocationIDs []string                          `json:"publish_invocation_ids"`
	Edges                []publicationEdge                 `json:"edges,omitempty"`
}

type publicationEdge struct {
	BeforeSHA string `json:"before_sha"`
	HeadSHA   string `json:"head_sha"`
}

func (proof *settlementPublication) projection() *workflow.ChangeProposal {
	if proof == nil {
		return nil
	}
	return &workflow.ChangeProposal{ID: proof.Proposal.PullRequestID, Number: proof.Proposal.PullRequestNumber, HeadSHA: proof.Proposal.HeadSHA, Open: true}
}

// deriveSettlementPublicationTx is shared by live, recovery and verifier
// settlement after their distinct stopped/drained fences have been validated.
// Only the exact Turn/epoch ledger (including admitted ancestor replays) is an
// authority. A webhook and arbitrary Participant history are never authorities.
func deriveSettlementPublicationTx(ctx context.Context, tx pgx.Tx, job Job, turn lockedTurn, role workflow.Role, snapshot workflow.Snapshot, observed *AgentTurnSettlementChangeProposal) (*settlementPublication, error) {
	return derivePublicationChainTx(ctx, tx, job, turn, role, snapshot, observed, 0, make(map[string]bool))
}

// Follow only explicit binding links. The invocation limit excludes later source
// operations which were not part of the recovered observation.
func derivePublicationChainTx(ctx context.Context, tx pgx.Tx, job Job, turn lockedTurn, role workflow.Role, snapshot workflow.Snapshot, observed *AgentTurnSettlementChangeProposal, limit int64, visiting map[string]bool) (*settlementPublication, error) {
	if visiting[turn.ID] {
		return nil, fmt.Errorf("%w: cyclic recovered publication chain", ErrAgentTurnPublicationConflict)
	}
	visiting[turn.ID] = true
	defer delete(visiting, turn.ID)
	if role != workflow.RoleDeveloper || snapshot.ChangeProposal != nil || turn.ChangeProposalID != "" {
		return nil, nil
	}
	if turn.MutationAdmissionOpen {
		return nil, ErrMutationAdmissionClosed
	}
	var unsettled bool
	if err := tx.QueryRow(ctx, unsettledMutationsForTurnSQL, turn.ID, turn.ExecutionEpoch).Scan(&unsettled); err != nil {
		return nil, err
	}
	if unsettled {
		return nil, ErrAgentTurnMutationsUnsettled
	}
	var boundHead, boundBranch, boundBase, boundPublishID, boundOpenID, boundNode string
	var boundPR, boundNumber int64
	boundErr := tx.QueryRow(ctx, `SELECT head_sha,head_ref,base_ref,source_publish_mutation_id::text,COALESCE(source_open_pr_mutation_id::text,''),COALESCE(pull_request_id,0),COALESCE(pull_request_number,0),COALESCE(pull_request_node_id,'') FROM agent_turn_publications WHERE agent_turn_id=$1 AND execution_epoch=$2`, turn.ID, turn.ExecutionEpoch).Scan(&boundHead, &boundBranch, &boundBase, &boundPublishID, &boundOpenID, &boundPR, &boundNumber, &boundNode)
	if boundErr != nil && !errors.Is(boundErr, pgx.ErrNoRows) {
		return nil, boundErr
	}
	query := mutationLedgerSelect
	if boundErr == nil {
		// A corroborated binding is already the starting publication authority.
		// Acknowledging old operation replays must not rewind it.
		query = mutationSelect + ` WHERE agent_turn_id=$1 AND execution_epoch=$2 AND kind='MUTATION' ORDER BY invocation_number`
	}
	rows, err := tx.Query(ctx, query, turn.ID, turn.ExecutionEpoch)
	if err != nil {
		return nil, err
	}
	var mutations []MutationReservation
	hasOpen := false
	for rows.Next() {
		m, err := scanMutation(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if (limit == 0 || m.InvocationNumber <= limit) && m.State == MutationSucceeded && (m.ToolName == "publish_changes" || m.ToolName == "open_pr") {
			mutations = append(mutations, m)
			hasOpen = hasOpen || m.ToolName == "open_pr"
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if !hasOpen && boundOpenID == "" {
		return nil, nil
	}
	var repositoryID, issueNumber int64
	var owner, name string
	if err := tx.QueryRow(ctx, `SELECT repository_id,repository_owner,repository_name,issue_number FROM workflows WHERE id=$1`, job.WorkflowID).Scan(&repositoryID, &owner, &name, &issueNumber); err != nil {
		return nil, err
	}
	branch := fmt.Sprintf("omnigrex/issue-%d", issueNumber)
	proof := &settlementPublication{}
	head := turn.ExpectedHeadSHA
	if boundErr == nil {
		// An open binding carries exact source operations, not readiness. A
		// branch-only binding can instead be opened genuinely in this Turn.
		if boundBranch != branch {
			return nil, ErrAgentTurnPublicationConflict
		}
		if boundOpenID != "" {
			recovered, err := recoveredOpenPublicationTx(ctx, tx, job, turn, boundPublishID, boundOpenID, visiting)
			if err != nil {
				return nil, err
			}
			if recovered == nil || recovered.OpenInvocationID != boundOpenID || recovered.Proposal.HeadSHA != boundHead || recovered.Proposal.HeadRef != boundBranch || recovered.Proposal.BaseRef != boundBase || recovered.Proposal.PullRequestID != boundPR || recovered.Proposal.PullRequestNumber != boundNumber || recovered.Proposal.PullRequestNodeID != boundNode {
				return nil, ErrAgentTurnPublicationConflict
			}
			proof = recovered
			head = boundHead
		} else {
			source, err := getMutationByID(ctx, tx, boundPublishID)
			if err != nil {
				return nil, err
			}
			var authorized bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_turns AS source JOIN agent_sessions AS session ON session.id=source.agent_session_id WHERE source.id=$1 AND source.workflow_id=$2 AND session.agent_participant_id=$3 AND source.id<>$4 AND source.completed_at IS NOT NULL AND source.created_at<=$5)`, source.AgentTurnID, job.WorkflowID, job.AgentParticipantID, turn.ID, turn.CreatedAt).Scan(&authorized); err != nil {
				return nil, err
			}
			var result struct {
				Head    string `json:"head"`
				Branch  string `json:"branch"`
				Changed *bool  `json:"changed"`
			}
			if !authorized || source.State != MutationSucceeded || source.ToolName != "publish_changes" || source.ExternalService != "git" || source.ExternalResourceID != fmt.Sprintf("%d:%s", repositoryID, branch) || !decodePublicationResult(source.Result, &result) || result.Changed == nil || !*result.Changed || result.Head != boundHead || result.Branch != branch || !publication.Revision(result.Head) || !publication.Revision(source.ExpectedSHA) || source.ExpectedSHA == result.Head || source.HistoryPublication && source.ProposedSHA != result.Head {
				return nil, ErrAgentTurnPublicationConflict
			}
			head = boundHead
			proof.PublishInvocationIDs = append(proof.PublishInvocationIDs, boundPublishID)
		}
	}
	visitedHeads := make(map[string]bool)
	for _, edge := range proof.Edges {
		visitedHeads[edge.BeforeSHA] = true
		visitedHeads[edge.HeadSHA] = true
	}
	if head != "" {
		visitedHeads[head] = true
	}
	var previous int64
	for _, m := range mutations {
		if m.AgentTurnID != turn.ID || m.ExecutionEpoch != turn.ExecutionEpoch || m.InvocationNumber <= previous || !publication.Revision(m.ExpectedSHA) {
			return nil, ErrAgentTurnPublicationConflict
		}
		if limit != 0 {
			var request struct {
				OperationID string `json:"operation_id"`
			}
			if m.OperationID == "" || m.FinishedAt == nil || json.Unmarshal(m.Request, &request) != nil || request.OperationID != m.OperationID {
				return nil, fmt.Errorf("%w: source chain contains incomplete operation evidence", ErrAgentTurnPublicationConflict)
			}
		}
		previous = m.InvocationNumber
		if head == "" {
			head = m.ExpectedSHA
			visitedHeads[head] = true
		}
		if m.ExpectedSHA != head {
			return nil, ErrAgentTurnPublicationConflict
		}
		switch m.ToolName {
		case "publish_changes":
			var result struct {
				Head    string `json:"head"`
				Branch  string `json:"branch"`
				Changed *bool  `json:"changed"`
			}
			if m.ExternalService != "git" || m.ExternalResourceID != fmt.Sprintf("%d:%s", repositoryID, branch) ||
				!decodePublicationResult(m.Result, &result) || result.Changed == nil || !publication.Revision(result.Head) || result.Branch != branch ||
				*result.Changed && result.Head == head || !*result.Changed && result.Head != head ||
				m.HistoryPublication && *result.Changed && m.ProposedSHA != result.Head || m.ProposedSHA != "" && (!*result.Changed || m.ProposedSHA != result.Head) {
				return nil, ErrAgentTurnPublicationConflict
			}
			if *result.Changed {
				if visitedHeads[result.Head] {
					return nil, ErrAgentTurnPublicationConflict
				}
				visitedHeads[result.Head] = true
			}
			proof.PublishInvocationIDs = append(proof.PublishInvocationIDs, m.ID)
			if proof.OpenInvocationID != "" && *result.Changed {
				proof.Edges = append(proof.Edges, publicationEdge{BeforeSHA: head, HeadSHA: result.Head})
			}
			head = result.Head
		case "open_pr":
			result, err := publication.DecodeOpenResult(m.Result)
			if err != nil {
				return nil, fmt.Errorf("%w: unsupported or incomplete open_pr association evidence", ErrAgentTurnPublicationConflict)
			}
			if proof.OpenInvocationID != "" || len(proof.PublishInvocationIDs) == 0 || m.ExternalService != "github" ||
				m.ExternalResourceID != fmt.Sprintf("%d:%s:%s", repositoryID, branch, result.BaseRef) || result.RepositoryID != repositoryID || result.RepositoryOwner != owner || result.RepositoryName != name || result.HeadRef != branch || result.HeadSHA != head || result.Status != "OPEN" || boundBase != "" && boundBase != result.BaseRef {
				return nil, ErrAgentTurnPublicationConflict
			}
			proof.OpenInvocationID = m.ID
			proof.Proposal = AgentTurnSettlementChangeProposal{WorkflowID: job.WorkflowID, RepositoryID: repositoryID, RepositoryOwner: owner, RepositoryName: name,
				PullRequestID: result.PullRequestID, PullRequestNumber: result.Number, PullRequestNodeID: result.NodeID, Status: result.Status, Active: true,
				BaseRef: result.BaseRef, BaseSHA: result.BaseSHA, HeadRef: result.HeadRef, HeadSHA: result.HeadSHA}
		}
	}
	if proof.OpenInvocationID == "" {
		return nil, ErrAgentTurnPublicationConflict
	}
	proof.Proposal.HeadSHA = head
	if observed != nil {
		// The base branch may have advanced since opening without retargeting the
		// PR. Readiness must agree on identity, refs and the proven published head,
		// not on that descriptive base commit observation.
		comparison := *observed
		comparison.BaseSHA = proof.Proposal.BaseSHA
		if comparison != proof.Proposal {
			return nil, ErrAgentTurnChangeProposalConflict
		}
	}
	return proof, nil
}

func decodePublicationResult(raw []byte, target any) bool {
	// Use the same strict object rules as publication results, including EOF.
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return false
	}
	// Publish results have exactly these three fields; no ignored metadata may
	// grant an association.
	if len(object) != 3 || object["head"] == nil || object["branch"] == nil || object["changed"] == nil {
		return false
	}
	return json.Unmarshal(raw, target) == nil
}

func encodeSettlementResult(result AgentTurnSettlement, proof *settlementPublication) ([]byte, error) {
	return json.Marshal(struct {
		AgentTurnSettlement
		Publication *settlementPublication `json:"publication_association,omitempty"`
	}{result, proof})
}

// Both endpoints must belong to completed, earlier executions of this exact
// Participant and Workflow. Recursing through that execution's own binding
// proves every intervening publication edge, not just matching endpoint heads.
func recoveredOpenPublicationTx(ctx context.Context, tx pgx.Tx, job Job, current lockedTurn, publishID, openID string, visiting map[string]bool) (*settlementPublication, error) {
	var sources []MutationReservation
	var turns []lockedTurn
	for _, id := range []string{publishID, openID} {
		m, err := getMutationByID(ctx, tx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: linked source operation is missing", ErrAgentTurnPublicationConflict)
		}
		if err != nil {
			return nil, err
		}
		var authorized bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_turns t JOIN agent_sessions s ON s.id=t.agent_session_id WHERE t.id=$1 AND t.workflow_id=$2 AND s.agent_participant_id=$3 AND t.id<>$4 AND t.completed_at IS NOT NULL AND t.created_at<=$5 AND t.execution_epoch=$6 AND EXISTS(SELECT 1 FROM tool_invocations m WHERE m.id=$7 AND m.kind='MUTATION'))`, m.AgentTurnID, job.WorkflowID, job.AgentParticipantID, current.ID, current.CreatedAt, m.ExecutionEpoch, m.ID).Scan(&authorized); err != nil {
			return nil, err
		}
		var request struct {
			OperationID string `json:"operation_id"`
		}
		if !authorized || m.State != MutationSucceeded || m.FinishedAt == nil || m.OperationID == "" || json.Unmarshal(m.Request, &request) != nil || request.OperationID != m.OperationID {
			return nil, fmt.Errorf("%w: linked source is not a completed, authorized, operation-matching publication", ErrAgentTurnPublicationConflict)
		}
		t, err := lockAgentTurnWithLock(ctx, tx, m.AgentTurnID, "")
		if err != nil {
			return nil, err
		}
		sources = append(sources, m)
		turns = append(turns, t)
	}
	if sources[0].ToolName != "publish_changes" || sources[1].ToolName != "open_pr" {
		return nil, ErrAgentTurnPublicationConflict
	}
	var published struct {
		Head    string `json:"head"`
		Branch  string `json:"branch"`
		Changed *bool  `json:"changed"`
	}
	if !decodePublicationResult(sources[0].Result, &published) || published.Changed == nil || !*published.Changed {
		return nil, ErrAgentTurnPublicationConflict
	}
	target := 0
	if turns[1].CreatedAt.After(turns[0].CreatedAt) || turns[0].ID == turns[1].ID && sources[1].InvocationNumber > sources[0].InvocationNumber {
		target = 1
	}
	proof, err := derivePublicationChainTx(ctx, tx, job, turns[target], workflow.RoleDeveloper, workflow.Snapshot{}, nil, sources[target].InvocationNumber, visiting)
	if errors.Is(err, ErrAgentTurnMutationsUnsettled) || errors.Is(err, ErrMutationAdmissionClosed) {
		return nil, ErrAgentTurnPublicationConflict
	}
	if err != nil {
		return nil, err
	}
	if proof == nil || proof.OpenInvocationID != openID || proof.Proposal.HeadSHA != published.Head {
		return nil, fmt.Errorf("%w: linked chain does not prove the opening operation and recovered head", ErrAgentTurnPublicationConflict)
	}
	found := false
	for _, id := range proof.PublishInvocationIDs {
		found = found || id == publishID
	}
	if !found {
		return nil, ErrAgentTurnPublicationConflict
	}
	return proof, nil
}

// Invalid recovered evidence is not authority, but must not replace an already
// permitted blocker/failure with a generic settlement retry. Keep the diagnostic
// separate from the original outcome, observation and Human Handoff.
func settlementPublicationForOutcomeTx(ctx context.Context, tx pgx.Tx, job Job, turn lockedTurn, role workflow.Role, snapshot workflow.Snapshot, observed *AgentTurnSettlementChangeProposal, outcome workflow.TurnOutcome) (*settlementPublication, string, error) {
	proof, err := deriveSettlementPublicationTx(ctx, tx, job, turn, role, snapshot, observed)
	if observed == nil && (outcome == workflow.TurnOutcomeBlocked || outcome == workflow.TurnOutcomeInfrastructureFailed || outcome == workflow.TurnOutcomePrerequisiteFailed) && errors.Is(err, ErrAgentTurnPublicationConflict) {
		var recovered bool
		if queryErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_turn_publications WHERE agent_turn_id=$1 AND execution_epoch=$2 AND source_open_pr_mutation_id IS NOT NULL)`, turn.ID, turn.ExecutionEpoch).Scan(&recovered); queryErr != nil {
			return nil, "", queryErr
		}
		if recovered {
			return nil, "Recovered Pull Request association could not be verified; no association recorded: " + err.Error(), nil
		}
	}
	return proof, "", err
}
