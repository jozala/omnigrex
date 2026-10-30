package mcp

import (
	"context"
	"encoding/json"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
)

// Production uses the same observer contract that a future scheduling policy
// can consume. Older test backends without it get explicit unavailable evidence,
// never an asserted empty policy or alternate read authority.
type ciEvidenceAPI interface {
	DiscoverRequiredCI(context.Context, string, string, string, string) githubapi.CIInventory
	ObserveCI(context.Context, string, string, string, string, int64) githubapi.CIObservation
	VerifyCITarget(context.Context, string, string, string, int64, int64, int, string, string, string) githubapi.CITargetMapping
	GetCheckRunMetadata(context.Context, string, string, string, int64) (githubapi.CheckRun, error)
}

func (backend *ProductionBackend) ciMapping(ctx context.Context, token string, scope ToolScope) githubapi.CITargetMapping {
	mapping := githubapi.CITargetMapping{RepositoryID: scope.Repository.ID, ReviewedHead: scope.HeadSHA, State: "head_only", ObservedAt: time.Now().UTC()}
	api, ok := backend.github.(ciEvidenceAPI)
	if !ok {
		mapping.State = "unavailable"
		return mapping
	}
	if scope.PullRequest == nil {
		return mapping
	}
	verified := api.VerifyCITarget(ctx, token, scope.Repository.Owner, scope.Repository.Name, scope.Repository.ID, scope.PullRequest.ID, int(scope.PullRequest.Number), scope.HeadSHA, scope.Branch, scope.DefaultBranch)
	if verified.RepositoryID != scope.Repository.ID || verified.ReviewedHead != scope.HeadSHA || verified.PullRequestID != scope.PullRequest.ID || int64(verified.PullRequestNumber) != scope.PullRequest.Number || verified.State == "verified" && (!validRevision(verified.TestMergeSHA) || verified.TestMergeSHA == scope.HeadSHA || !validRevision(verified.BaseSHA) || verified.BaseRef != scope.DefaultBranch) {
		mapping.State = "unavailable"
		mapping.Failures = []githubapi.CIEvidenceFailure{{Operation: "test_merge_mapping", Code: "inconsistent_response"}}
		return mapping
	}
	return verified
}
func allowsCIHead(mapping githubapi.CITargetMapping, head string) bool {
	return head == mapping.ReviewedHead || mapping.State == "verified" && mapping.TestMergeSHA != "" && head == mapping.TestMergeSHA
}
func allowsCIRun(mapping githubapi.CITargetMapping, run githubapi.CIRun) bool {
	if !allowsCIHead(mapping, run.HeadSHA) || run.Event == "merge_group" || run.RepositoryID != 0 && run.RepositoryID != mapping.RepositoryID {
		return false
	}
	if run.HeadSHA == mapping.ReviewedHead {
		return true
	}
	// Missing provider identity never proves alternative target/source access.
	return run.RepositoryID == mapping.RepositoryID && run.Event == "pull_request"
}
func marshalCIResult(value any, mapping githubapi.CITargetMapping) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, mapCIError(err)
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(encoded, &result) != nil {
		return nil, newCIDiagnosticError(CIDiagnosticUnavailable, "unavailable", 0, "")
	}
	provenance, _ := json.Marshal(mapping)
	result["target_mapping"] = provenance
	encoded, err = json.Marshal(result)
	if err != nil {
		return nil, mapCIError(err)
	}
	if len(encoded) > githubapi.MaxCISerializedResponseBytes {
		return nil, newCIDiagnosticError(CIDiagnosticResourceExhausted, "resource_exhausted", 0, "")
	}
	return encoded, nil
}

func (backend *ProductionBackend) getCIEvidence(ctx context.Context, invocation Invocation) (json.RawMessage, error) {
	var arguments listRunsArguments
	if decodeCIArguments(stripReservedSignatureArgument(invocation), &arguments) != nil || len(arguments.Cursor) > 4096 {
		return nil, newCIDiagnosticError(CIDiagnosticInvalidContinuation, "invalid_arguments", 0, "")
	}
	token, err := backend.ciCredential(ctx, invocation.Scope)
	if err != nil {
		return nil, err
	}
	scope := invocation.Scope
	mapping := backend.ciMapping(ctx, token, scope)
	inventory := githubapi.CIInventory{Availability: githubapi.CIUnavailable, Branch: scope.DefaultBranch, ObservedAt: time.Now().UTC(), Requirements: []githubapi.CIRequirement{}, Unresolved: []githubapi.CIUnresolvedRule{}, Failures: []githubapi.CIEvidenceFailure{{Operation: "policy", Code: "unavailable"}}}
	var observed []githubapi.CIObservation
	if api, ok := backend.github.(ciEvidenceAPI); ok {
		inventory = api.DiscoverRequiredCI(ctx, token, scope.Repository.Owner, scope.Repository.Name, scope.DefaultBranch)
		observed = append(observed, api.ObserveCI(ctx, token, scope.Repository.Owner, scope.Repository.Name, scope.HeadSHA, scope.Repository.ID))
		if mapping.State == "verified" {
			observed = append(observed, api.ObserveCI(ctx, token, scope.Repository.Owner, scope.Repository.Name, mapping.TestMergeSHA, scope.Repository.ID))
		}
	} else {
		checks, err := backend.github.GetCheckRuns(ctx, token, scope.Repository.Owner, scope.Repository.Name, scope.HeadSHA)
		if err != nil {
			return nil, newGitHubReadError(err)
		}
		observed = []githubapi.CIObservation{{Availability: githubapi.CIPartial, RepositoryID: scope.Repository.ID, HeadSHA: scope.HeadSHA, ObservedAt: time.Now().UTC(), Checks: checks, Statuses: []githubapi.CommitStatus{}, Failures: []githubapi.CIEvidenceFailure{{Operation: "statuses", Code: "unavailable"}}}}
	}
	checkHeads := map[int64]string{}
	statusHeads := map[int64]string{}
	for _, target := range observed {
		if !allowsCIHead(mapping, target.HeadSHA) || target.RepositoryID != scope.Repository.ID {
			return nil, ErrToolPrecondition
		}
		for _, check := range target.Checks {
			if check.HeadSHA != target.HeadSHA || checkHeads[check.ID] != "" && checkHeads[check.ID] != target.HeadSHA {
				return nil, ErrToolPrecondition
			}
			checkHeads[check.ID] = target.HeadSHA
		}
		for _, status := range target.Statuses {
			if statusHeads[status.ID] != "" && statusHeads[status.ID] != target.HeadSHA {
				return nil, ErrToolPrecondition
			}
			statusHeads[status.ID] = target.HeadSHA
		}
	}
	page, err := githubapi.PageCIEvidence(inventory, observed, mapping, arguments.Cursor)
	if err != nil {
		return nil, mapCIError(err)
	}
	return json.Marshal(page)
}
