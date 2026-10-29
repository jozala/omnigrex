package agentturn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jozala/omnigrex/internal/agentprofile"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/reviewpolicy"
	"github.com/jozala/omnigrex/internal/store"
)

type ReviewAdmissionStore interface {
	BeginReviewAdmission(context.Context, store.JobLease) (store.ReviewAdmission, error)
	RecordReviewAdmission(context.Context, store.JobLease, string, store.ReviewAdmissionObservation) (store.ReviewAdmission, error)
	DeferReviewAdmission(context.Context, store.JobLease, time.Duration) error
	GetActiveChangeProposal(context.Context, string) (*store.ActiveChangeProposal, error)
}
type ReviewCIObserver interface {
	DiscoverRequiredCI(context.Context, string, string, string, string) githubapi.CIInventory
	ObserveCI(context.Context, string, string, string, string, int64) githubapi.CIObservation
	VerifyCITarget(context.Context, string, string, string, int64, int64, int, string, string, string) githubapi.CITargetMapping
	GetPullRequest(context.Context, string, string, string, int) (githubapi.PullRequest, error)
}

// ReviewGate owns no Runtime Process and performs all network work outside store transactions.
type ReviewGate struct {
	Store    ReviewAdmissionStore
	Source   agentprofile.Source
	Observer ReviewCIObserver
	Now      func() time.Time
}
type reviewStartContext struct {
	RequestID       string                        `json:"request_id"`
	Generation      int64                         `json:"target_generation"`
	ReviewedHead    string                        `json:"reviewed_head"`
	Mapping         githubapi.CITargetMapping     `json:"target_mapping"`
	PolicySourceSHA string                        `json:"policy_source_sha"`
	Inventory       githubapi.CIInventory         `json:"expected_ci"`
	Targets         []reviewTargetContext         `json:"targets"`
	Reason          string                        `json:"release_reason"`
	ObservedAt      time.Time                     `json:"observed_at"`
	Deadline        time.Time                     `json:"deadline"`
	Failures        []githubapi.CIEvidenceFailure `json:"failures,omitempty"`
}
type reviewTargetContext struct {
	Commit       string                          `json:"commit"`
	ObservedAt   time.Time                       `json:"observed_at"`
	Availability githubapi.CIAvailability        `json:"availability"`
	Results      []githubapi.CIRequirementResult `json:"results"`
	Failures     []githubapi.CIEvidenceFailure   `json:"failures,omitempty"`
}

func (g *ReviewGate) Check(ctx context.Context, request Request, r store.ReviewAdmission) (store.ReviewAdmission, error) {
	now := time.Now().UTC()
	if g.Now != nil {
		now = g.Now().UTC()
	}
	durable, err := g.Store.GetActiveChangeProposal(ctx, request.Lease.WorkflowID)
	if err != nil {
		return r, err
	}
	if durable == nil {
		return r, ErrChangeProposalVerificationFailed
	}
	if durable.RepositoryOwner != request.RepositoryOwner || durable.RepositoryName != request.RepositoryName || durable.PullRequestID != r.PullRequestID || durable.PullRequestNumber != r.PullRequestNumber || durable.RepositoryID != r.RepositoryID {
		return r, ErrChangeProposalVerificationFailed
	}
	pr, err := g.Observer.GetPullRequest(ctx, request.InstallationCredential, request.RepositoryOwner, request.RepositoryName, int(durable.PullRequestNumber))
	if err != nil {
		return g.unavailable(ctx, request, r, now, "pull_request")
	}
	if pr.Base.Repository == nil || pr.Base.Repository.ID != r.RepositoryID || pr.Base.Repository.FullName != request.RepositoryOwner+"/"+request.RepositoryName {
		return r, ErrChangeProposalVerificationFailed
	}
	head, err := VerifyObservedChangeProposal(durable.PullRequestID, durable.PullRequestNumber, durable.HeadRef, durable.BaseRef, pr)
	if err != nil {
		return r, err
	}
	policy := reviewpolicy.Default()
	var inventory githubapi.CIInventory
	sourceSHA := r.PolicySourceSHA
	if len(r.Policy) > 0 {
		if json.Unmarshal(r.Policy, &policy) != nil {
			return r, errors.New("invalid durable review admission snapshot")
		}
	} else {
		sourceSHA, policy, err = g.loadPolicy(ctx, request, r.PolicySourceSHA)
		if errors.Is(err, reviewpolicy.ErrInvalid) {
			return r, permanentError{cause: err}
		}
		if err != nil {
			r.PolicySourceSHA = sourceSHA
			return g.unavailable(ctx, request, r, now, "policy_snapshot")
		}
	}
	if len(r.Inventory) > 0 {
		if json.Unmarshal(r.Inventory, &inventory) != nil {
			return r, errors.New("invalid durable review inventory")
		}
	} else {
		inventory = githubapi.CIInventory{Availability: githubapi.CIAvailable, ObservedAt: now, Repository: request.RepositoryOwner + "/" + request.RepositoryName, Branch: durable.BaseRef}
		if policy.Start != "immediate" && policy.Selection == "github_required" {
			inventory = g.Observer.DiscoverRequiredCI(ctx, request.InstallationCredential, request.RepositoryOwner, request.RepositoryName, durable.BaseRef)
		}
		if policy.Selection == "explicit" {
			for _, selector := range policy.Checks {
				inventory.Requirements = append(inventory.Requirements, githubapi.CIRequirement{Context: selector.Context, AppID: selector.AppID, Source: "repository_explicit", SourceType: selector.Source})
			}
		}
	}
	deadline := r.Deadline
	if len(r.Policy) == 0 {
		deadline = r.CreatedAt.Add(policy.Timeout)
	}
	mapping := githubapi.CITargetMapping{RepositoryID: durable.RepositoryID, PullRequestID: durable.PullRequestID, PullRequestNumber: int(durable.PullRequestNumber), ReviewedHead: head, BaseSHA: pr.Base.SHA, BaseRef: pr.Base.Ref, ObservedAt: now, State: "not_observed_immediate"}
	var observations []githubapi.CIObservation
	if policy.Start != "immediate" {
		mapping = g.Observer.VerifyCITarget(ctx, request.InstallationCredential, request.RepositoryOwner, request.RepositoryName, durable.RepositoryID, durable.PullRequestID, int(durable.PullRequestNumber), head, durable.HeadRef, durable.BaseRef)
		observations = append(observations, g.Observer.ObserveCI(ctx, request.InstallationCredential, request.RepositoryOwner, request.RepositoryName, head, durable.RepositoryID))
		if mapping.State == "verified" && mapping.TestMergeSHA != "" {
			observations = append(observations, g.Observer.ObserveCI(ctx, request.InstallationCredential, request.RepositoryOwner, request.RepositoryName, mapping.TestMergeSHA, durable.RepositoryID))
		}
	}
	evaluationRequest := r
	if r.HeadSHA != head || r.BaseSHA != mapping.BaseSHA || r.TestMergeSHA != mapping.TestMergeSHA {
		evaluationRequest.Generation++
		if r.HeadSHA != head || len(r.Observation) > 0 {
			evaluationRequest.GraceAnchor = now
			if now.After(deadline) {
				evaluationRequest.GraceAnchor = deadline
			}
		}
	}
	evidence := evaluateReviewCI(policy, inventory, mapping, observations, evaluationRequest, now, deadline)
	evidence.PolicySourceSHA = sourceSHA
	// Never issue release evidence for an obsolete generation. The store will
	// retarget under the original generation fence and apply the replacement grace.
	raw, _ := json.Marshal(compactReviewContext(evidence))
	normalized, _ := json.Marshal(policy)
	var frozen json.RawMessage
	if inventory.Availability == githubapi.CIAvailable && len(inventory.Failures) == 0 {
		frozen, _ = json.Marshal(inventory)
	}
	return g.Store.RecordReviewAdmission(ctx, request.Lease, r.ID, store.ReviewAdmissionObservation{Generation: r.Generation, HeadSHA: head, BaseSHA: mapping.BaseSHA, TestMergeSHA: mapping.TestMergeSHA, PolicySourceSHA: sourceSHA, Policy: normalized, Inventory: frozen, Evidence: raw, Release: evidence.Reason != "waiting"})
}
func (g *ReviewGate) loadPolicy(ctx context.Context, r Request, frozenSHA string) (string, reviewpolicy.Policy, error) {
	sha := frozenSHA
	var err error
	if sha == "" {
		sha, err = g.Source.ResolveDefaultBranchCommit(ctx, r.InstallationCredential, r.RepositoryOwner, r.RepositoryName)
	}
	if err != nil {
		return "", reviewpolicy.Policy{}, err
	}
	policy, err := reviewpolicy.LoadAtCommit(ctx, g.Source, r.InstallationCredential, r.RepositoryOwner, r.RepositoryName, sha)
	return sha, policy, err
}
func (g *ReviewGate) unavailable(ctx context.Context, request Request, r store.ReviewAdmission, now time.Time, operation string) (store.ReviewAdmission, error) {
	evidence := reviewStartContext{RequestID: r.ID, Generation: r.Generation, ReviewedHead: r.HeadSHA, ObservedAt: now, Deadline: r.Deadline, Reason: "waiting", Failures: []githubapi.CIEvidenceFailure{{Operation: operation, Code: "unavailable"}}}
	if !now.Before(r.Deadline) {
		evidence.Reason = "timeout_incomplete"
	}
	raw, _ := json.Marshal(compactReviewContext(evidence))
	return g.Store.RecordReviewAdmission(ctx, request.Lease, r.ID, store.ReviewAdmissionObservation{Generation: r.Generation, HeadSHA: r.HeadSHA, BaseSHA: r.BaseSHA, TestMergeSHA: r.TestMergeSHA, PolicySourceSHA: r.PolicySourceSHA, Evidence: raw, Release: evidence.Reason != "waiting"})
}

// evaluateReviewCI distinguishes terminal completion from success and availability.
// A missing item on one authorized commit does not erase observed evidence on
// another, while any newer pending producer result on either keeps the wait.
func evaluateReviewCI(p reviewpolicy.Policy, inventory githubapi.CIInventory, mapping githubapi.CITargetMapping, observations []githubapi.CIObservation, r store.ReviewAdmission, now, deadline time.Time) reviewStartContext {
	e := reviewStartContext{RequestID: r.ID, Generation: r.Generation, ReviewedHead: mapping.ReviewedHead, Mapping: mapping, Inventory: inventory, Reason: "waiting", ObservedAt: now, Deadline: deadline, Failures: append([]githubapi.CIEvidenceFailure(nil), mapping.Failures...)}
	if p.Start == "immediate" {
		e.Reason = "immediate"
		return e
	}
	available := len(mapping.Failures) == 0 && inventory.Availability == githubapi.CIAvailable && len(inventory.Failures) == 0 && len(inventory.Unresolved) == 0 && (mapping.State == "verified" || mapping.State == "head_only")
	fallback := available && len(inventory.Requirements) == 0 && p.Selection == "github_required"
	if fallback {
		seen := map[string]bool{}
		for _, o := range observations {
			for _, c := range o.Checks {
				if !seen[c.Name] {
					inventory.Requirements = append(inventory.Requirements, githubapi.CIRequirement{Context: c.Name, Source: "all_observed"})
					seen[c.Name] = true
				}
			}
			for _, s := range o.Statuses {
				if !seen[s.Context] {
					inventory.Requirements = append(inventory.Requirements, githubapi.CIRequirement{Context: s.Context, Source: "all_observed"})
					seen[s.Context] = true
				}
			}
		}
	}
	allComplete := available && len(observations) > 0
	for _, o := range observations {
		if p.Selection != "explicit" && (o.Availability != githubapi.CIAvailable || len(o.Failures) > 0) {
			allComplete = false
		}
		target := reviewTargetContext{Commit: o.HeadSHA, ObservedAt: o.ObservedAt, Availability: o.Availability, Failures: o.Failures}
		for _, required := range inventory.Requirements {
			selected := o
			// Selection narrows the shared matching contract; it never substitutes a
			// status creator for an App or invents a second producer matcher.
			if required.Source == "repository_explicit" {
				if required.SourceType == "check" {
					selected.Statuses = nil
				}
				if required.SourceType == "status" {
					selected.Checks = nil
				}
				selected.Failures = nil
				selected.Availability = githubapi.CIAvailable
				operation := "checks"
				if required.SourceType == "status" {
					operation = "statuses"
				}
				for _, failure := range o.Failures {
					if failure.Operation == operation || failure.Operation == "repository" {
						selected.Failures = append(selected.Failures, failure)
						selected.Availability = githubapi.CIPartial
					}
				}
				if o.Availability == githubapi.CIUnavailable || o.Availability != githubapi.CIAvailable && len(o.Failures) == 0 {
					selected.Availability = githubapi.CIUnavailable
				}

			}
			results := githubapi.MatchRequiredCI(githubapi.CIInventory{Requirements: []githubapi.CIRequirement{required}}, selected)
			target.Results = append(target.Results, results...)
		}
		e.Targets = append(e.Targets, target)
	}
	for _, required := range inventory.Requirements {
		found := false
		for _, t := range e.Targets {
			for _, result := range t.Results {
				if result.Requirement != required {
					continue
				}
				switch result.State {
				case githubapi.CITerminalSuccess, githubapi.CITerminalNonSuccess:
					found = true
				case githubapi.CIMissing:
				default:
					allComplete = false
				}
			}
		}
		if !found {
			allComplete = false
		}
	}
	graceDone := !now.Before(r.GraceAnchor.Add(p.RegistrationGrace))
	if allComplete && (!fallback || graceDone) {
		e.Reason = "completed_ci"
		if fallback && len(inventory.Requirements) == 0 {
			e.Reason = "no_ci_after_grace"
		}
	}
	if e.Reason == "waiting" && !now.Before(deadline) {
		e.Reason = "timeout_incomplete"
	}
	return e
}

func (g *ReviewGate) validate() error {
	if g == nil || g.Store == nil || g.Source == nil || g.Observer == nil {
		return fmt.Errorf("%w: review gate", ErrDependencyNil)
	}
	return nil
}

// Keep prompts and durable observations bounded; the frozen inventory remains
// complete in its own request column and detailed evidence stays in CI tools.
func compactReviewContext(e reviewStartContext) reviewStartContext {
	// Copy slices before trimming strings: compact prompt projection must not
	// mutate the frozen inventory used for subsequent observations.
	e.Inventory.Requirements = append([]githubapi.CIRequirement(nil), e.Inventory.Requirements...)
	e.Inventory.Unresolved = append([]githubapi.CIUnresolvedRule(nil), e.Inventory.Unresolved...)
	e.Targets = append([]reviewTargetContext(nil), e.Targets...)
	const limit = 32
	truncated := false
	if len(e.Inventory.Requirements) > limit {
		e.Inventory.Requirements = e.Inventory.Requirements[:limit]
		truncated = true
	}
	if len(e.Inventory.Unresolved) > limit {
		e.Inventory.Unresolved = e.Inventory.Unresolved[:limit]
		truncated = true
	}
	for i := range e.Targets {
		e.Targets[i].Results = append([]githubapi.CIRequirementResult(nil), e.Targets[i].Results...)
		if len(e.Targets[i].Results) > limit {
			e.Targets[i].Results = e.Targets[i].Results[:limit]
			truncated = true
		}
	}
	// An exceptionally large provider-supplied context/source must not make the
	// launch envelope unbounded. Preserve the release reason and mapping, mark
	// truncation, and refer to the existing tools and complete frozen inventory.
	raw, _ := json.Marshal(e)
	if len(raw) > 48<<10 {
		e.Inventory.Requirements = nil
		e.Inventory.Unresolved = nil
		for i := range e.Targets {
			e.Targets[i].Results = nil
		}
		truncated = true
	}
	if truncated {
		e.Failures = append(e.Failures, githubapi.CIEvidenceFailure{Operation: "context", Code: "details_truncated_use_ci_tools"})
	}
	return e
}
