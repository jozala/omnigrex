package agentturn

import (
	"fmt"

	githubapi "github.com/jozala/omnigrex/internal/github"
)

// VerifyObservedChangeProposal reconciles a freshly fetched GitHub Pull Request
// against the durable current Change Proposal association. It accepts a
// changed commit head and returns it; identity changes and closed or merged
// Pull Requests, as well as head/base branch changes, fail permanently so the
// caller creates an actionable Human Handoff without adopting, reopening, or
// retargeting work. Callers additionally confirm the repository match from
// the fetch coordinates before invoking this check. Preparation and
// terminal-intent revalidation share this path so both observe identical
// acceptance policy.
func VerifyObservedChangeProposal(pullRequestID, pullRequestNumber int64, headRef, baseRef string, observed githubapi.PullRequest) (string, error) {
	if observed.ID != pullRequestID || int64(observed.Number) != pullRequestNumber {
		return "", fmt.Errorf("%w: Pull Request identity changed (durable %d/%d, observed %d/%d); do not adopt, reopen, or retarget",
			ErrChangeProposalVerificationFailed, pullRequestID, pullRequestNumber, observed.ID, observed.Number)
	}
	if observed.State != "open" {
		return "", fmt.Errorf("%w: Pull Request %d is not open (state %q, merged=%v); do not adopt, reopen, or retarget",
			ErrChangeProposalVerificationFailed, observed.Number, observed.State, observed.Merged)
	}
	if observed.Head.Ref != headRef {
		return "", fmt.Errorf("%w: Pull Request head branch changed (durable %q, observed %q); do not retarget",
			ErrChangeProposalVerificationFailed, headRef, observed.Head.Ref)
	}
	if observed.Base.Ref != baseRef {
		return "", fmt.Errorf("%w: Pull Request base branch changed (durable %q, observed %q); do not retarget",
			ErrChangeProposalVerificationFailed, baseRef, observed.Base.Ref)
	}
	return observed.Head.SHA, nil
}
