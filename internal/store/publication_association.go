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
	var boundHead, boundBranch, boundBase, boundPublishID, boundOpenID string
	boundErr := tx.QueryRow(ctx, `SELECT head_sha,head_ref,base_ref,source_publish_mutation_id::text,COALESCE(source_open_pr_mutation_id::text,'') FROM agent_turn_publications WHERE agent_turn_id=$1 AND execution_epoch=$2`, turn.ID, turn.ExecutionEpoch).Scan(&boundHead, &boundBranch, &boundBase, &boundPublishID, &boundOpenID)
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
		if m.State == MutationSucceeded && (m.ToolName == "publish_changes" || m.ToolName == "open_pr") {
			mutations = append(mutations, m)
			hasOpen = hasOpen || m.ToolName == "open_pr"
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if !hasOpen {
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
		// New-format completed settlements already associate opened PRs. A bound
		// branch with no PR may legitimately be opened by this later Turn without
		// publishing another commit first. Validate only that exact bound source.
		if boundOpenID != "" || boundBranch != branch {
			return nil, ErrAgentTurnPublicationConflict
		}
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
	visitedHeads := make(map[string]bool)
	if head != "" {
		visitedHeads[head] = true
	}
	var previous int64
	for _, m := range mutations {
		if m.AgentTurnID != turn.ID || m.ExecutionEpoch != turn.ExecutionEpoch || m.InvocationNumber <= previous || !publication.Revision(m.ExpectedSHA) {
			return nil, ErrAgentTurnPublicationConflict
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
