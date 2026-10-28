package github_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
)

func TestProgressCommentUpdateChecksLocationAndMarkerBeforeMutation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		number  int
		marker  bool
		success bool
	}{{"scoped", 17, true, true}, {"wrong parent", 18, true, false}, {"unowned", 17, false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			marker, _ := githubapi.RenderMarker(githubapi.Marker{WorkflowID: "workflow", OperationID: "progress"})
			updates := 0
			client, done := ciClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/acme/widgets/issues/comments/777" {
					t.Errorf("unexpected path: %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				body := "old"
				if tc.marker {
					body = githubapi.JoinBodyParts(body, marker)
				}
				if r.Method == http.MethodPatch {
					updates++
					var payload struct {
						Body string `json:"body"`
					}
					if json.NewDecoder(r.Body).Decode(&payload) != nil {
						t.Error("invalid update payload")
					}
					body = payload.Body
				} else if r.Method != http.MethodGet {
					t.Errorf("unexpected method: %s", r.Method)
				}
				comment := githubapi.IssueComment{ID: 777, NodeID: "IC_777", Body: body, HTMLURL: "https://github.test/acme/widgets/issues/17#issuecomment-777", IssueURL: fmt.Sprintf("https://api.github.test/repos/acme/widgets/issues/%d", tc.number), CreatedAt: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 10, 27, 12, 1, 0, 0, time.UTC)}
				_ = json.NewEncoder(w).Encode(comment)
			}))
			defer done()
			comment, err := client.UpdateIssueComment(context.Background(), "token", "acme", "widgets", 17, 777, githubapi.CommentRequest{Body: "new", Marker: marker})
			if tc.success {
				if err != nil || updates != 1 || comment.Body != githubapi.JoinBodyParts("new", marker) {
					t.Fatalf("update=%+v, calls=%d, err=%v", comment, updates, err)
				}
			} else if err == nil || updates != 0 {
				t.Fatalf("out-of-scope update: calls=%d err=%v", updates, err)
			}
		})
	}
}

func TestCITargetReportsUnknownMergeabilityWithoutGrantingAlternateReads(t *testing.T) {
	client, done := ciClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widgets/pulls/17" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"id":17,"node_id":"PR_17","number":17,"title":"fixture","state":"open","html_url":"https://github.test/acme/widgets/pull/17","head":{"ref":"feature","sha":%q},"base":{"ref":"main","sha":%q,"repo":{"id":9123,"full_name":"acme/widgets"}},"mergeable":null}`, ciHeadSHA, ciOtherSHA)
	}))
	defer done()
	mapping := client.VerifyCITarget(context.Background(), "token", "acme", "widgets", 9123, 17, 17, ciHeadSHA, "feature", "main")
	if mapping.State != "head_only" || mapping.TestMergeSHA != "" || len(mapping.Failures) != 1 || mapping.Failures[0].Code != "mergeability_pending" {
		t.Fatalf("unknown mapping = %+v", mapping)
	}
}
