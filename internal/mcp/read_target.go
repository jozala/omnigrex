package mcp

import (
	"context"
	"fmt"

	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

// publicationReadTarget is derived observation context, never token authority or
// mutation fencing. Only the gateway can attach it to an Invocation.
type publicationReadTarget struct {
	HeadSHA     string
	PullRequest *PullRequestScope
}

func publicationBoundRead(name string) bool {
	switch name {
	case ToolGetPullRequest, ToolListPullRequestReviews, ToolListReviewThreads,
		ToolGetCheckRuns, ToolGetCheckRunDiagnostics, ToolListCIRuns, ToolGetCIRun,
		ToolGetCIJobLogs, ToolSearchCIJobLogs:
		return true
	default:
		return false
	}
}

// resolvePublicationRead uses only the exact live Turn/epoch ledger, including
// acknowledged ancestor replays. It deliberately does not consult GitHub's
// current branch tip or the process-local mutation planning maps.
func (backend *ProductionBackend) resolvePublicationRead(scope ToolScope, mutations []store.MutationReservation) (publicationReadTarget, error) {
	target := publicationReadTarget{HeadSHA: scope.HeadSHA, PullRequest: cloneToolScope(scope).PullRequest}
	if scope.Role != workflow.RoleDeveloper {
		return target, nil
	}
	var previous int64
	for _, mutation := range mutations {
		if mutation.AgentTurnID != scope.AgentTurnID || mutation.ExecutionEpoch != scope.ExecutionEpoch ||
			mutation.State != store.MutationSucceeded || mutation.ID == "" || mutation.InvocationNumber <= previous {
			return publicationReadTarget{}, ErrToolPrecondition
		}
		previous = mutation.InvocationNumber
		metadata := replayMetadataForScope(mutation.ToolName, scope)
		if mutation.ExternalService != metadata.ExternalService || mutation.ExternalResourceID != metadata.ExternalResourceID || !validRevision(mutation.ExpectedSHA) {
			return publicationReadTarget{}, ErrToolPrecondition
		}
		switch mutation.ToolName {
		case ToolPublishChanges:
			var result struct {
				Head    string `json:"head"`
				Branch  string `json:"branch"`
				Changed *bool  `json:"changed"`
			}
			if !decodeExactResult(mutation.Result, &result) || result.Changed == nil || !validRevision(result.Head) || result.Branch != scope.Branch ||
				mutation.HistoryPublication && *result.Changed && mutation.ProposedSHA == "" ||
				mutation.ProposedSHA != "" && (!*result.Changed || mutation.ProposedSHA != result.Head) ||
				*result.Changed && result.Head == mutation.ExpectedSHA || !*result.Changed && result.Head != mutation.ExpectedSHA {
				return publicationReadTarget{}, ErrToolPrecondition
			}
			// Verified preparation can repair an existing PR retry's starting head
			// before the publication that produced it is acknowledged as a replay.
			// Accept that fully validated, already-applied result without moving the
			// target; a mismatched predecessor may never grant a different head.
			if mutation.ExpectedSHA != target.HeadSHA && result.Head != target.HeadSHA {
				return publicationReadTarget{}, ErrToolPrecondition
			}
			target.HeadSHA = result.Head
		case ToolOpenPR:
			pr, _, ok := restoredPullRequest(mutation.Result, target.HeadSHA)
			if !ok || mutation.ExpectedSHA != target.HeadSHA || !sameOrMissingPullRequest(target.PullRequest, pr) {
				return publicationReadTarget{}, ErrToolPrecondition
			}
			target.PullRequest = pr
		default:
			return publicationReadTarget{}, fmt.Errorf("%w: non-publication read evidence", ErrToolPrecondition)
		}
	}
	return target, nil
}

// publicationReadStore must validate the live fence and select only successful
// publication evidence for this exact Turn/epoch. Replays are included only when
// the Turn has not already bound a recovered publication as its starting scope.
type publicationReadStore interface {
	ListAgentTurnPublicationReads(context.Context, store.AgentTurnLease, bool) ([]store.MutationReservation, error)
}

type publicationReadResolver interface {
	resolvePublicationRead(ToolScope, []store.MutationReservation) (publicationReadTarget, error)
}

func (gateway *Gateway) publicationReadTarget(ctx context.Context, resolver publicationReadResolver, invocation Invocation) (*publicationReadTarget, error) {
	if invocation.Scope.Role != workflow.RoleDeveloper {
		return nil, nil
	}
	ledger, ok := gateway.store.(publicationReadStore)
	if !ok {
		return nil, ErrToolDependency
	}
	mutations, err := ledger.ListAgentTurnPublicationReads(ctx, invocation.lease, !invocation.Scope.RecoveredPublication)
	if err != nil {
		return nil, ErrToolDependency
	}
	target, err := resolver.resolvePublicationRead(invocation.Scope, mutations)
	if err != nil {
		return nil, err
	}
	return &target, nil
}
