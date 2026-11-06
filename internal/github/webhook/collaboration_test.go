package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/collaboration"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/store"
)

type collaborationFixture struct {
	state                            collaboration.State
	authorized, active, loseResponse bool
	comments                         []githubapi.IssueComment
	calls                            int
	listFailures                     int
	postErrors                       []error
	posts                            int
}

func (f *collaborationFixture) FindCollaborationWorkItem(context.Context, int64, int64, int64) (string, error) {
	return "11111111-1111-4111-8111-111111111111", nil
}
func (f *collaborationFixture) AdmitHumanInput(_ context.Context, in store.HumanInputSubmission) (collaboration.Receipt, error) {
	f.calls++
	id := fmt.Sprintf("A00000000-0000-4000-8000-%012d", f.calls)
	return f.state.Accept(id, in.Command, collaboration.Admission{Active: f.active, Authorized: in.Authorized, ParticipantID: "developer", Actor: in.Actor, Source: in.Source, At: in.At}), nil
}
func (f *collaborationFixture) RepositoryCredential(context.Context, string, string) (string, error) {
	return "fixture", nil
}
func (f *collaborationFixture) CanDecideWorkItem(context.Context, string, string, string, string, int64) (bool, error) {
	return f.authorized, nil
}
func (f *collaborationFixture) ListIssueComments(context.Context, string, string, string, int) ([]githubapi.IssueComment, error) {
	if f.listFailures > 0 {
		f.listFailures--
		return nil, errors.New("temporary read failure")
	}
	return f.comments, nil
}
func (f *collaborationFixture) CreateIssueComment(_ context.Context, _ string, _, _ string, _ int, in githubapi.CommentRequest) (githubapi.IssueComment, error) {
	f.posts++
	if len(f.postErrors) > 0 {
		err := f.postErrors[0]
		f.postErrors = f.postErrors[1:]
		return githubapi.IssueComment{}, err
	}
	comment := githubapi.IssueComment{ID: int64(len(f.comments) + 1), Body: githubapi.JoinBodyParts(in.Body, in.Marker)}
	f.comments = append(f.comments, comment)
	if f.loseResponse {
		f.loseResponse = false
		return githubapi.IssueComment{}, errors.New("lost response")
	}
	return comment, nil
}
func collaborationClaim(action, body string) store.WebhookClaim {
	raw, _ := json.Marshal(map[string]any{"action": action, "repository": map[string]any{"id": 1, "name": "repo", "owner": map[string]any{"login": "owner"}}, "issue": map[string]any{"id": 2, "number": 3}, "sender": map[string]any{"id": 42}, "comment": map[string]any{"id": 7, "body": body, "html_url": "https://github.com/owner/repo/issues/3#issuecomment-7", "created_at": time.Now(), "user": map[string]any{"id": 42, "login": "maintainer", "type": "User"}}})
	return store.WebhookClaim{WebhookDelivery: store.WebhookDelivery{EventName: "issue_comment", Action: action, RepositoryID: 1, RepositoryOwner: "owner", RepositoryName: "repo", IssueID: 2, IssueNumber: 3, Payload: raw}, ReceivedAt: time.Now()}
}
func TestIssueCommandsQueueOnceAndReconcileAcknowledgment(t *testing.T) {
	f := &collaborationFixture{authorized: true, active: true, loseResponse: true}
	adapter := &IssueCollaboration{Store: f, API: f, Credentials: f}
	claim := collaborationClaim("created", "/omnigrex steer\nPreserve compatibility.")
	if _, err := adapter.HandleCollaboration(context.Background(), claim); err == nil {
		t.Fatal("expected lost acknowledgment response")
	}
	if len(f.state.Inputs) != 1 || f.state.Inputs[0].Body != "Preserve compatibility." || len(f.comments) != 1 {
		t.Fatal("accepted content lost")
	}
	if _, err := adapter.HandleCollaboration(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if len(f.state.Inputs) != 1 || len(f.comments) != 1 {
		t.Fatal("duplicate input or acknowledgment")
	}
	original := f.state.Inputs[0]
	for _, action := range []string{"edited", "deleted"} {
		if _, err := adapter.HandleCollaboration(context.Background(), collaborationClaim(action, "/omnigrex steer\nReplacement")); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.state.Inputs) != 1 || f.state.Inputs[0] != original {
		t.Fatal("edit or deletion rewrote history")
	}
}
func TestIssueCommandsRejectUnauthorizedAndLifecycleWithoutPrompting(t *testing.T) {
	for _, authorized := range []bool{false, true} {
		f := &collaborationFixture{authorized: authorized, active: false}
		adapter := &IssueCollaboration{Store: f, API: f, Credentials: f}
		if _, err := adapter.HandleCollaboration(context.Background(), collaborationClaim("created", "/omnigrex steer\nContinue")); err != nil {
			t.Fatal(err)
		}
		if len(f.state.Inputs) != 0 || len(f.comments) != 1 || f.state.Receipts[0].ResponseID != "" {
			t.Fatal("rejection did not preserve lifecycle")
		}
	}
}

func (f *collaborationFixture) ReserveCollaborationReceipt(_ context.Context, _, id string, a collaboration.RenderedPublication) (collaboration.RenderedPublication, uint64, error) {
	for i := range f.state.Receipts {
		r := &f.state.Receipts[i]
		if r.ID == id {
			if r.Artifact != nil && !r.PublicationRejected {
				return *r.Artifact, 0, nil
			}
			if r.Artifact == nil {
				r.Artifact = &a
			}
			r.PublicationAttempt++
			r.PublicationRejected = false
			return *r.Artifact, r.PublicationAttempt, nil
		}
	}
	return a, 0, collaboration.ErrInvalid
}

func TestAcknowledgmentReadFailureDoesNotConsumePublicationOwnership(t *testing.T) {
	f := &collaborationFixture{authorized: true, active: true, listFailures: 1}
	adapter := &IssueCollaboration{Store: f, API: f, Credentials: f}
	claim := collaborationClaim("created", "/omnigrex steer\nKeep the bounded scope.")
	if _, err := adapter.HandleCollaboration(t.Context(), claim); err == nil {
		t.Fatal("expected read failure")
	}
	if len(f.state.Inputs) != 1 || len(f.comments) != 0 || f.state.Receipts[0].Artifact != nil {
		t.Fatal("read failure consumed POST ownership")
	}
	if _, err := adapter.HandleCollaboration(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	if len(f.state.Inputs) != 1 || len(f.comments) != 1 || f.state.Inputs[0].Reference != "A1" {
		t.Fatal("retry duplicated admission or lost receipt")
	}
}

func TestAcknowledgmentPostRejectionAndUncertainty(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"rate limit", &githubapi.RateLimitError{APIError: &githubapi.APIError{StatusCode: 429}}, true},
		{"validation", &githubapi.APIError{StatusCode: 422}, true},
		{"permission", &githubapi.PermissionError{APIError: &githubapi.APIError{StatusCode: 403}}, true},
		{"transport", errors.New("lost response; no artifact visible yet"), false},
		{"server", &githubapi.TransientError{Cause: &githubapi.APIError{StatusCode: 503}}, false},
		{"timeout", &githubapi.TransientError{Cause: &githubapi.APIError{StatusCode: 408}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &collaborationFixture{authorized: true, active: true, postErrors: []error{tc.err}}
			adapter := &IssueCollaboration{Store: f, API: f, Credentials: f}
			claim := collaborationClaim("created", "/omnigrex steer\nKeep scope.")
			if _, err := adapter.HandleCollaboration(t.Context(), claim); err == nil {
				t.Fatal("expected first POST failure")
			}
			frozen := *f.state.Receipts[0].Artifact
			_, err := adapter.HandleCollaboration(t.Context(), claim)
			if tc.retry {
				if err != nil || f.posts != 2 || len(f.comments) != 1 || f.comments[0].Body != frozen.Body {
					t.Fatal("definite rejection could not safely retry", err, f.posts, f.comments)
				}
			} else if err == nil || f.posts != 1 || len(f.comments) != 0 {
				t.Fatal("uncertain effect blindly reposted", err, f.posts)
			}
			if len(f.state.Inputs) != 1 {
				t.Fatal("retry duplicated admitted input")
			}
		})
	}
}

func (f *collaborationFixture) RejectCollaborationReceipt(_ context.Context, _, id string, attempt uint64) error {
	for i := range f.state.Receipts {
		r := &f.state.Receipts[i]
		if r.ID == id && r.PublicationAttempt == attempt && attempt != 0 {
			r.PublicationRejected = true
			return nil
		}
	}
	return collaboration.ErrInvalid
}
