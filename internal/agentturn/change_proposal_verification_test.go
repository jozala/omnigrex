package agentturn_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/agentturn"
	githubapi "github.com/jozala/omnigrex/internal/github"
)

func verificationObservedPullRequest(id int64, number int, state, headRef, headSHA, baseRef string) githubapi.PullRequest {
	return githubapi.PullRequest{
		ID: id, NodeID: "PR_node", Number: number, Title: "Change", State: state,
		Head: githubapi.PullRequestBranch{Ref: headRef, SHA: headSHA},
		Base: githubapi.PullRequestBranch{Ref: baseRef, SHA: "base-sha"},
	}
}

func TestVerifyObservedChangeProposalAcceptsChangedHead(t *testing.T) {
	observed := verificationObservedPullRequest(64, 12, "open", "feature", "0123456789abcdef0123456789abcdef01234567", "main")
	head, err := agentturn.VerifyObservedChangeProposal(64, 12, "feature", "main", observed)
	if err != nil {
		t.Fatalf("VerifyObservedChangeProposal() error = %v", err)
	}
	if head != "0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("verified head = %q, want the changed GitHub head", head)
	}
}

func TestVerifyObservedChangeProposalRejectsRetargetedPullRequest(t *testing.T) {
	for _, test := range []struct {
		name        string
		mutate      func(*githubapi.PullRequest)
		wantContain string
	}{
		{name: "identity", mutate: func(pr *githubapi.PullRequest) { pr.ID = 65 }, wantContain: "identity changed"},
		{name: "closed", mutate: func(pr *githubapi.PullRequest) { pr.State = "closed" }, wantContain: "not open"},
		{name: "head branch", mutate: func(pr *githubapi.PullRequest) { pr.Head.Ref = "other" }, wantContain: "head branch changed"},
		{name: "base branch", mutate: func(pr *githubapi.PullRequest) { pr.Base.Ref = "other" }, wantContain: "base branch changed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			observed := verificationObservedPullRequest(64, 12, "open", "feature", "0123456789abcdef0123456789abcdef01234567", "main")
			test.mutate(&observed)
			if _, err := agentturn.VerifyObservedChangeProposal(64, 12, "feature", "main", observed); !errors.Is(err, agentturn.ErrChangeProposalVerificationFailed) || !strings.Contains(err.Error(), test.wantContain) {
				t.Fatalf("VerifyObservedChangeProposal() error = %v, want verification failure containing %q", err, test.wantContain)
			}
		})
	}
}
