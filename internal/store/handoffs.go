package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ReviewHandoff is the latest completed handoff for an exact Workflow, PR, and head.
type ReviewHandoff struct {
	ID                   string    `json:"id"`
	HeadSHA              string    `json:"head_sha"`
	Summary              string    `json:"summary"`
	PublicationConfirmed bool      `json:"publication_confirmed"`
	CreatedAt            time.Time `json:"created_at"`
}

func (store *Store) GetReviewHandoff(ctx context.Context, workflowID string, repositoryID, pullRequestID int64, head string) (*ReviewHandoff, error) {
	if !validUUID(workflowID) || repositoryID <= 0 || pullRequestID <= 0 || strings.TrimSpace(head) == "" {
		return nil, errors.New("invalid handoff scope")
	}
	var handoff ReviewHandoff
	err := store.pool.QueryRow(ctx, `
SELECT invocation.id, invocation.expected_sha, invocation.request->>'summary',
       invocation.external_service = 'github', invocation.admitted_at
FROM tool_invocations AS invocation
JOIN agent_turns AS turn ON turn.id = invocation.agent_turn_id
JOIN workflow_attempts AS attempt ON attempt.id = turn.workflow_attempt_id
JOIN workflows AS workflow ON workflow.id = attempt.workflow_id
WHERE workflow.id = $1 AND workflow.repository_id = $2
  AND invocation.kind = 'MUTATION' AND invocation.tool_name = 'request_review'
  AND invocation.state = 'SUCCEEDED' AND invocation.external_service IN ('github', 'omnigrex')
  AND invocation.expected_sha = $4 AND invocation.result->>'head_sha' = $4
  AND invocation.result->>'pull_request_id' = $3
  AND invocation.result->>'outcome' = 'REVIEW_REQUESTED'
  AND length(btrim(invocation.request->>'summary')) > 0
ORDER BY invocation.admitted_at DESC, invocation.id DESC
LIMIT 1`, workflowID, repositoryID, strconv.FormatInt(pullRequestID, 10), head).Scan(
		&handoff.ID, &handoff.HeadSHA, &handoff.Summary, &handoff.PublicationConfirmed, &handoff.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read scoped review handoff: %w", err)
	}
	return &handoff, nil
}
