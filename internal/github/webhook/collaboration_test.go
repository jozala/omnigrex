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
	return f.comments, nil
}
func (f *collaborationFixture) CreateIssueComment(_ context.Context, _ string, _, _ string, _ int, in githubapi.CommentRequest) (githubapi.IssueComment, error) {
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
