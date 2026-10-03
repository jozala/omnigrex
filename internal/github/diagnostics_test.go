package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	githubapi "github.com/jozala/omnigrex/internal/github"
)

const diagnosticRequestID = "A41A:2AF0FB:6AAAD0:6A71D9:6ABC2962"

func diagnosticThreadFixture() map[string]any {
	return reviewThreadsGraphQLFixture(1, graphQLPageFixture(false, false, "thread", "thread"), []any{
		reviewThreadGraphQLFixture("PRRT_401", false, false, 1, graphQLPageFixture(false, false, "comment", "comment"), []any{
			reviewCommentGraphQLFixture("PRRC_401", 401, "reviewer", "private-content-sentinel", false, false),
		}),
	})
}

func setDiagnosticFixture(data map[string]any, path string, value any) {
	parts := strings.Split(path, "/")
	var current any = data
	for _, part := range parts[:len(parts)-1] {
		if part == "0" {
			current = current.([]any)[0]
		} else {
			current = current.(map[string]any)[part]
		}
	}
	current.(map[string]any)[parts[len(parts)-1]] = value
}

func TestReadValidationDiagnosticsThroughHTTP(t *testing.T) {
	const pr = "repository/pullRequest/"
	const threads = pr + "reviewThreads/"
	const thread = threads + "nodes/0/"
	const comments = thread + "comments/"
	const comment = comments + "nodes/0/"
	for _, test := range []struct {
		path          string
		value         any
		reason, field string
	}{
		{"repository", nil, "missing_field", "repository"},
		{"repository/id", "bad\nprivate-content-sentinel", "invalid_node_id", "repository.id"},
		{"repository/name", "private-content-sentinel", "identity_mismatch", "repository.name"},
		{"repository/owner", nil, "missing_field", "repository.owner"},
		{"repository/owner/login", "private-content-sentinel", "identity_mismatch", "repository.owner.login"},
		{"repository/pullRequest", nil, "missing_field", "pull_request"},
		{pr + "id", "", "invalid_node_id", "pull_request.id"},
		{pr + "number", nil, "missing_field", "pull_request.number"},
		{pr + "number", 24, "identity_mismatch", "pull_request.number"},
		{pr + "repository", nil, "missing_field", "pull_request.repository"},
		{pr + "repository/id", "other", "identity_mismatch", "pull_request.repository.id"},
		{pr + "repository/name", "other", "identity_mismatch", "pull_request.repository.name"},
		{pr + "repository/owner", nil, "missing_field", "pull_request.repository.owner"},
		{pr + "repository/owner/login", "other", "identity_mismatch", "pull_request.repository.owner.login"},
		{pr + "reviewThreads", nil, "missing_field", "review_threads"},
		{threads + "totalCount", nil, "missing_field", "review_threads.totalCount"},
		{threads + "totalCount", -1, "invalid_count", "review_threads.totalCount"},
		{threads + "totalCount", 0, "count_mismatch", "review_threads.totalCount"},
		{threads + "totalCount", 2, "count_mismatch", "review_threads.totalCount"},
		{threads + "nodes", nil, "missing_field", "review_threads.nodes"},
		{threads + "nodes", []any{nil}, "missing_field", "review_thread"},
		{threads + "pageInfo", nil, "missing_field", "page_info"},
		{threads + "pageInfo/hasNextPage", nil, "missing_field", "page_info.hasNextPage"},
		{threads + "pageInfo/hasPreviousPage", nil, "missing_field", "page_info.hasPreviousPage"},
		{threads + "pageInfo/startCursor", nil, "missing_field", "page_info.startCursor"},
		{threads + "pageInfo/endCursor", nil, "missing_field", "page_info.endCursor"},
		{threads + "pageInfo/startCursor", "", "invalid_cursor", "page_info.startCursor"},
		{threads + "pageInfo/endCursor", "bad\n", "invalid_cursor", "page_info.endCursor"},
		{thread + "id", "", "invalid_node_id", "review_thread.id"},
		{thread + "isResolved", nil, "missing_field", "review_thread.isResolved"},
		{thread + "isOutdated", nil, "missing_field", "review_thread.isOutdated"},
		{thread + "path", "../private-content-sentinel", "invalid_path", "review_thread.path"},
		{thread + "comments", nil, "missing_field", "review_comments"},
		{comments + "totalCount", nil, "missing_field", "review_comments.totalCount"},
		{comments + "totalCount", 0, "invalid_count", "review_comments.totalCount"},
		{comments + "totalCount", 2, "count_mismatch", "review_comments.totalCount"},
		{comments + "nodes", nil, "missing_field", "review_comments.nodes"},
		{comments + "nodes", []any{nil}, "missing_field", "review_comment"},
		{thread + "diffSide", "BOTH", "invalid_diff_side", "review_thread.diffSide"},
		{thread + "subjectType", "OTHER", "invalid_subject_type", "review_thread.subjectType"},
		{thread + "originalLine", nil, "missing_field", "review_thread.originalLine"},
		{thread + "originalLine", 0, "invalid_line", "review_thread.originalLine"},
		{thread + "originalStartLine", 42, "invalid_range", "review_thread.originalStartLine"},
		{thread + "line", nil, "missing_field", "review_thread.line"},
		{thread + "line", 0, "invalid_line", "review_thread.line"},
		{thread + "isOutdated", true, "outdated_has_line", "review_thread.line"},
		{thread + "startDiffSide", "RIGHT", "single_line_has_diff_side", "review_thread.startDiffSide"},
		{thread + "startLine", 1, "single_line_has_start_line", "review_thread.startLine"},
		{comment + "id", "", "invalid_node_id", "review_comment.id"},
		{comment + "fullDatabaseId", nil, "missing_field", "review_comment.fullDatabaseId"},
		{comment + "fullDatabaseId", "0", "invalid_id", "review_comment.fullDatabaseId"},
		{comment + "body", " ", "missing_field", "review_comment.body"},
		{comment + "path", "private-content-sentinel", "location_mismatch", "review_comment.path"},
		{comment + "commit", nil, "missing_field", "review_comment.commit"},
		{comment + "commit/oid", "private-content-sentinel", "invalid_commit", "review_comment.commit.oid"},
		{comment + "originalCommit", nil, "missing_field", "review_comment.originalCommit"},
		{comment + "originalCommit/oid", "private-content-sentinel", "invalid_commit", "review_comment.originalCommit.oid"},
		{comment + "url", "https://github.test/private-content-sentinel", "invalid_url", "review_comment.url"},
		{comment + "author", nil, "missing_field", "review_comment.author"},
		{comment + "author/login", "", "invalid_identity", "review_comment.author.login"},
		{comment + "createdAt", nil, "missing_field", "review_comment.createdAt"},
		{comment + "createdAt", "0001-01-01T00:00:00Z", "invalid_timestamp", "review_comment.createdAt"},
		{comment + "updatedAt", nil, "missing_field", "review_comment.updatedAt"},
		{comment + "updatedAt", "0001-01-01T00:00:00Z", "invalid_timestamp", "review_comment.updatedAt"},
		{comment + "updatedAt", "2020-01-01T00:00:00Z", "timestamp_order", "review_comment.updatedAt"},
		{comment + "pullRequestReview/fullDatabaseId", nil, "missing_field", "review_comment.pullRequestReview.fullDatabaseId"},
		{comment + "pullRequestReview/fullDatabaseId", "0", "invalid_id", "review_comment.pullRequestReview.fullDatabaseId"},
		{comment + "position", 0, "invalid_position", "review_comment.position"},
		{comment + "subjectType", "FILE", "location_mismatch", "review_comment.subjectType"},
		{comment + "line", 999, "location_mismatch", "review_comment.line"},
		{comment + "startLine", 1, "location_mismatch", "review_comment.startLine"},
		{comment + "originalLine", 999, "location_mismatch", "review_comment.originalLine"},
		{comment + "originalStartLine", 1, "location_mismatch", "review_comment.originalStartLine"},
		{comment + "replyTo", map[string]any{"id": ""}, "invalid_node_id", "review_comment.replyTo.id"},
		{comment + "replyTo", map[string]any{"id": "other"}, "missing_field", "review_comment.replyTo.fullDatabaseId"},
		{comment + "replyTo", map[string]any{"id": "other", "fullDatabaseId": "0"}, "invalid_id", "review_comment.replyTo.fullDatabaseId"},
		{comment + "replyTo", map[string]any{"id": "other", "fullDatabaseId": "1"}, "missing_target", "review_comment.replyTo"},
	} {
		t.Run(test.path+"/"+test.reason, func(t *testing.T) {
			data := diagnosticThreadFixture()
			setDiagnosticFixture(data, test.path, test.value)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-GitHub-Request-Id", diagnosticRequestID)
				writeGraphQLData(t, w, data)
			}))
			defer server.Close()
			client, err := githubapi.NewAPIClient(server.Client(), server.URL)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.ListReviewThreads(context.Background(), "credential-sentinel", "acme", "widgets", 23)
			assertDiagnostic(t, err, test.reason, test.field, 200, diagnosticRequestID)
		})
	}
}

func assertDiagnostic(t *testing.T, err error, reason, field string, status int, requestID string) {
	t.Helper()
	if !errors.Is(err, githubapi.ErrInvalidAPIResponse) {
		t.Fatalf("expected invalid response, got %v", err)
	}
	detail := githubapi.SafeFailureDiagnostics(err)
	if detail.Reason != reason || detail.Field != field || detail.HTTPStatus != status || detail.RequestID != requestID || detail.Stage == "" {
		t.Fatalf("diagnostic = %+v, want %s %s status=%d request=%s", detail, reason, field, status, requestID)
	}
	encoded, _ := json.Marshal(detail)
	for _, secret := range []string{"credential-sentinel", "private-content-sentinel"} {
		if strings.Contains(string(encoded)+err.Error(), secret) {
			t.Fatalf("disclosed %s", secret)
		}
	}
}

func TestIssueDiagnosticsAndValidationOrder(t *testing.T) {
	for _, test := range []struct {
		field  string
		value  any
		reason string
	}{
		{"id", 0, "invalid_id"}, {"node_id", "", "missing_field"}, {"number", 13, "identity_mismatch"},
		{"title", " ", "missing_field"}, {"state", "private-content-sentinel", "invalid_state"},
		{"html_url", "private-content-sentinel", "invalid_url"}, {"html_url", "https://github.test/acme/other/issues/12", "identity_mismatch"},
	} {
		t.Run(test.field+test.reason, func(t *testing.T) {
			issue := map[string]any{"id": 123, "node_id": "I_123", "number": 12, "title": "private-content-sentinel", "state": "open", "html_url": "https://github.test/acme/widgets/issues/12"}
			issue[test.field] = test.value
			// An earlier defect must win over this additional last-field defect.
			if test.field != "html_url" {
				issue["html_url"] = "private-content-sentinel"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-GitHub-Request-Id", diagnosticRequestID)
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(issue)
			}))
			defer server.Close()
			client, err := githubapi.NewAPIClient(server.Client(), server.URL)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.GetIssue(context.Background(), "credential-sentinel", "acme", "widgets", 12)
			assertDiagnostic(t, err, test.reason, "issue."+test.field, 201, diagnosticRequestID)
		})
	}
}

func TestGraphQLDecodingAndEnvelopeDiagnostics(t *testing.T) {
	for _, test := range []struct {
		body, stage, reason string
		graphql             bool
	}{
		{`{"data":`, "response_decoding", "invalid_json", false},
		{`{"data":{"repository":"private-content-sentinel"}}`, "graphql_data_decoding", "invalid_json", false},
		{`{"data":null}`, "graphql_envelope", "missing_data", true},
		{`{}`, "graphql_envelope", "missing_data", true},
		{`{"data":null,"errors":[{"message":"credential-sentinel private-content-sentinel"}]}`, "graphql_envelope", "graphql_errors", true},
	} {
		t.Run(test.body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-GitHub-Request-Id", diagnosticRequestID)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			client, err := githubapi.NewAPIClient(server.Client(), server.URL)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.ListReviewThreads(context.Background(), "credential-sentinel", "acme", "widgets", 23)
			assertDiagnostic(t, err, test.reason, "", 200, diagnosticRequestID)
			if githubapi.SafeFailureDiagnostics(err).Stage != test.stage || errors.Is(err, githubapi.ErrGraphQLQueryFailed) != test.graphql {
				t.Fatalf("stage or sentinel changed: %v", err)
			}
		})
	}
}

type diagnosticFailingBody struct{ err error }

func (body diagnosticFailingBody) Read([]byte) (int, error) { return 0, body.err }
func (diagnosticFailingBody) Close() error                  { return nil }

func TestCancellationDiagnosticsPreserveRetrySemantics(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded,
		fmt.Errorf("credential-sentinel: %w", context.Canceled), fmt.Errorf("credential-sentinel: %w", context.DeadlineExceeded)} {
		for _, duringBody := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/body=%v", cause, duringBody), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				client, err := githubapi.NewAPIClient(httpDoerFunc(func(*http.Request) (*http.Response, error) {
					if !duringBody {
						return nil, cause
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"X-Github-Request-Id": []string{diagnosticRequestID}}, Body: diagnosticFailingBody{cause}}, nil
				}), "https://github.test")
				if err != nil {
					t.Fatal(err)
				}
				_, err = client.GetIssue(ctx, "credential-sentinel", "acme", "widgets", 12)
				detail := githubapi.SafeFailureDiagnostics(err)
				reason := detail.Reason
				if reason != githubapi.CancellationReason(cause) {
					t.Fatalf("cancellation lost: %+v %v", detail, err)
				}
				metadata := githubapi.ExtractSafeErrorMetadata(err)
				if duringBody {
					if !errors.Is(err, githubapi.ErrInvalidAPIResponse) || metadata.Transient || detail.HTTPStatus != 200 || detail.Stage != "response_decoding" {
						t.Fatalf("body retry/sentinel semantics changed: %+v %+v", metadata, detail)
					}
				} else if !metadata.Transient || detail.HTTPStatus != 0 || detail.RequestID != "" {
					t.Fatalf("transport retry/response metadata changed: %+v %+v", metadata, detail)
				}
			})
		}
	}
}

func TestContinuationMetadataNeverInheritsEarlierResponse(t *testing.T) {
	for _, kind := range []string{"validation", "transport", "decode", "count", "identity"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			client, err := githubapi.NewAPIClient(httpDoerFunc(func(*http.Request) (*http.Response, error) {
				calls++
				if calls == 2 && kind == "transport" {
					return nil, errors.New("private-content-sentinel")
				}
				data := diagnosticThreadFixture()
				thread := firstReviewThreadFixture(data)
				comments := thread["comments"].(map[string]any)
				comments["totalCount"] = 2
				header := make(http.Header)
				var payload any = data
				if calls == 1 {
					header.Set("X-GitHub-Request-Id", diagnosticRequestID)
					comments["pageInfo"] = graphQLPageFixture(true, false, "first", "next")
				} else {
					payload = map[string]any{"node": thread}
					switch kind {
					case "validation":
						thread["originalLine"] = nil
					case "decode":
						payload = map[string]any{"node": "private-content-sentinel"}
					case "count":
						comments["totalCount"] = 3
					case "identity":
						thread["pullRequest"].(map[string]any)["number"] = 24
					}
				}
				body, _ := json.Marshal(map[string]any{"data": payload})
				return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			}), "https://github.test")
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.ListReviewThreads(context.Background(), "credential-sentinel", "acme", "widgets", 23)
			if err == nil || calls != 2 {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
			detail := githubapi.SafeFailureDiagnostics(err)
			if detail.RequestID != "" {
				t.Fatalf("inherited request ID: %+v", detail)
			}
			if kind == "transport" {
				if detail.HTTPStatus != 0 {
					t.Fatalf("inherited status: %+v", detail)
				}
			} else if detail.HTTPStatus != 200 {
				t.Fatalf("lost response: %+v", detail)
			}
			if kind == "validation" && (detail.Reason != "missing_field" || detail.Field != "review_thread.originalLine") {
				t.Fatalf("lost precise validation: %+v", detail)
			}
		})
	}
}

func TestReviewLocationPaginationAndReplyRuleDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name, reason, field string
		mutate              func(map[string]any)
	}{
		{"oversized threads", "oversized_page", "review_threads.nodes", func(data map[string]any) {
			connection := data["repository"].(map[string]any)["pullRequest"].(map[string]any)["reviewThreads"].(map[string]any)
			connection["nodes"], connection["totalCount"] = make([]any, 101), 101
		}},
		{"oversized comments", "oversized_page", "review_comments.nodes", func(data map[string]any) {
			comments := firstReviewThreadFixture(data)["comments"].(map[string]any)
			comments["nodes"], comments["totalCount"] = make([]any, 101), 101
		}},
		{"comments exceed count", "count_mismatch", "review_comments.totalCount", func(data map[string]any) {
			comments := firstReviewThreadFixture(data)["comments"].(map[string]any)
			comments["nodes"] = []any{firstReviewCommentFixture(data), firstReviewCommentFixture(data)}
		}},
		{"duplicate comment node", "duplicate_id", "review_comment.id", func(data map[string]any) {
			comments := firstReviewThreadFixture(data)["comments"].(map[string]any)
			comments["nodes"], comments["totalCount"] = []any{firstReviewCommentFixture(data), firstReviewCommentFixture(data)}, 2
		}},
		{"duplicate comment database ID", "duplicate_id", "review_comment.fullDatabaseId", func(data map[string]any) {
			comments := firstReviewThreadFixture(data)["comments"].(map[string]any)
			comments["nodes"], comments["totalCount"] = []any{firstReviewCommentFixture(data), reviewCommentGraphQLFixture("PRRC_402", 401, "reviewer", "Reply", false, false)}, 2
		}},
		{"duplicate thread", "duplicate_id", "review_thread.id", func(data map[string]any) {
			connection := data["repository"].(map[string]any)["pullRequest"].(map[string]any)["reviewThreads"].(map[string]any)
			connection["nodes"], connection["totalCount"] = []any{firstReviewThreadFixture(data), firstReviewThreadFixture(data)}, 2
		}},
		{"reply mismatched database ID", "identity_mismatch", "review_comment.replyTo.fullDatabaseId", func(data map[string]any) {
			comments := firstReviewThreadFixture(data)["comments"].(map[string]any)
			reply := reviewCommentGraphQLFixture("PRRC_402", 402, "developer", "Reply", false, true)
			reply["replyTo"].(map[string]any)["fullDatabaseId"] = "999"
			comments["nodes"], comments["totalCount"] = []any{firstReviewCommentFixture(data), reply}, 2
		}},
		{"reply not to root", "reply_not_to_root", "review_comment.replyTo", func(data map[string]any) {
			comments := firstReviewThreadFixture(data)["comments"].(map[string]any)
			comments["nodes"], comments["totalCount"] = []any{firstReviewCommentFixture(data), reviewCommentGraphQLFixture("PRRC_402", 402, "developer", "Reply", false, false)}, 2
		}},
		{"root is reply", "root_is_reply", "review_comment.replyTo", func(data map[string]any) {
			connection := data["repository"].(map[string]any)["pullRequest"].(map[string]any)["reviewThreads"].(map[string]any)
			second := reviewThreadGraphQLFixture("PRRT_402", false, false, 1, graphQLPageFixture(false, false, "second", "second"), []any{reviewCommentGraphQLFixture("PRRC_402", 402, "reviewer", "Reply", false, true)})
			connection["nodes"], connection["totalCount"] = []any{firstReviewThreadFixture(data), second}, 2
		}},
		{"outdated start line", "outdated_has_line", "review_thread.startLine", func(data map[string]any) {
			thread := firstReviewThreadFixture(data)
			thread["isOutdated"], thread["line"], thread["startLine"] = true, nil, 1
		}},
		{"range missing side", "missing_field", "review_thread.startDiffSide", func(data map[string]any) { firstReviewThreadFixture(data)["originalStartLine"] = 1 }},
		{"range invalid side", "invalid_diff_side", "review_thread.startDiffSide", func(data map[string]any) {
			thread := firstReviewThreadFixture(data)
			thread["originalStartLine"], thread["startDiffSide"] = 1, "BOTH"
		}},
		{"range missing current start", "missing_field", "review_thread.startLine", func(data map[string]any) {
			thread := firstReviewThreadFixture(data)
			thread["originalStartLine"], thread["startDiffSide"] = 1, "RIGHT"
		}},
		{"range nonpositive current start", "invalid_line", "review_thread.startLine", func(data map[string]any) {
			thread := firstReviewThreadFixture(data)
			thread["originalStartLine"], thread["startDiffSide"], thread["startLine"] = 1, "RIGHT", 0
		}},
		{"range reversed current start", "invalid_range", "review_thread.startLine", func(data map[string]any) {
			thread := firstReviewThreadFixture(data)
			thread["originalStartLine"], thread["startDiffSide"], thread["startLine"] = 1, "RIGHT", 999
		}},
		{"nonpositive original range start", "invalid_line", "review_thread.originalStartLine", func(data map[string]any) {
			firstReviewThreadFixture(data)["originalStartLine"] = 0
		}},
		{"file comment has position", "position_on_file", "review_comment.position", func(data map[string]any) {
			for _, item := range []map[string]any{firstReviewThreadFixture(data), firstReviewCommentFixture(data)} {
				item["subjectType"] = "FILE"
				for _, field := range []string{"line", "startLine", "originalLine", "originalStartLine", "startDiffSide"} {
					item[field] = nil
				}
			}
			firstReviewCommentFixture(data)["position"] = 1
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := diagnosticThreadFixture()
			test.mutate(data)
			client, err := githubapi.NewAPIClient(httpDoerFunc(func(*http.Request) (*http.Response, error) {
				body, _ := json.Marshal(map[string]any{"data": data})
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			}), "https://github.test")
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.ListReviewThreads(context.Background(), "credential-sentinel", "acme", "widgets", 23)
			assertDiagnostic(t, err, test.reason, test.field, 200, "")
		})
	}
	for _, field := range []string{"line", "startLine", "originalLine", "originalStartLine", "startDiffSide"} {
		t.Run("file thread/"+field, func(t *testing.T) {
			data := diagnosticThreadFixture()
			thread := firstReviewThreadFixture(data)
			thread["subjectType"] = "FILE"
			for _, name := range []string{"line", "startLine", "originalLine", "originalStartLine", "startDiffSide"} {
				thread[name] = nil
			}
			thread[field] = 1
			reason := "file_has_line"
			if field == "startDiffSide" {
				thread[field], reason = "RIGHT", "file_has_diff_side"
			}
			client, err := githubapi.NewAPIClient(httpDoerFunc(func(*http.Request) (*http.Response, error) {
				body, _ := json.Marshal(map[string]any{"data": data})
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			}), "https://github.test")
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.ListReviewThreads(context.Background(), "credential-sentinel", "acme", "widgets", 23)
			assertDiagnostic(t, err, reason, "review_thread."+field, 200, "")
		})
	}
}

func TestReviewQueryCancellationRetainsOperationStage(t *testing.T) {
	for _, continuation := range []bool{false, true} {
		for _, duringBody := range []bool{false, true} {
			t.Run(fmt.Sprintf("continuation=%v/body=%v", continuation, duringBody), func(t *testing.T) {
				calls := 0
				client, err := githubapi.NewAPIClient(httpDoerFunc(func(*http.Request) (*http.Response, error) {
					calls++
					if calls == 1 && continuation {
						data := diagnosticThreadFixture()
						comments := firstReviewThreadFixture(data)["comments"].(map[string]any)
						comments["totalCount"], comments["pageInfo"] = 2, graphQLPageFixture(true, false, "first", "next")
						body, _ := json.Marshal(map[string]any{"data": data})
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
					}
					cause := fmt.Errorf("credential-sentinel: %w", context.DeadlineExceeded)
					if !duringBody {
						return nil, cause
					}
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: diagnosticFailingBody{cause}}, nil
				}), "https://github.test")
				if err != nil {
					t.Fatal(err)
				}
				_, err = client.ListReviewThreads(context.Background(), "credential-sentinel", "acme", "widgets", 23)
				detail := githubapi.SafeFailureDiagnostics(err)
				wantStage := "review_threads_query"
				if continuation {
					wantStage = "review_comments_query"
				}
				if detail.Stage != wantStage || detail.Reason != "deadline_exceeded" {
					t.Fatalf("operation/cancellation lost: %+v", detail)
				}
			})
		}
	}
}

func TestThreadPaginationDiagnosticsUseFailingPage(t *testing.T) {
	const lastID = "B41B:2BF0FB:6BBBD0:6B71D9:6BBC2962"
	for _, test := range []struct {
		name, reason, field string
		pages               int
		mutate              func(map[string]any)
	}{
		{"count changed", "count_changed", "review_threads.totalCount", 2, func(data map[string]any) {
			setDiagnosticFixture(data, "repository/pullRequest/reviewThreads/totalCount", 4)
		}},
		{"repository changed", "identity_changed", "repository.id", 2, func(data map[string]any) {
			setDiagnosticFixture(data, "repository/id", "R_changed")
			setDiagnosticFixture(data, "repository/pullRequest/repository/id", "R_changed")
		}},
		{"PR changed", "identity_changed", "pull_request.id", 2, func(data map[string]any) { setDiagnosticFixture(data, "repository/pullRequest/id", "PR_changed") }},
		{"start repeats previous", "repeated_cursor", "page_info.startCursor", 2, func(data map[string]any) {
			setDiagnosticFixture(data, "repository/pullRequest/reviewThreads/pageInfo/startCursor", "cursor-1")
		}},
		{"end repeats previous", "repeated_cursor", "page_info.endCursor", 2, func(data map[string]any) {
			setDiagnosticFixture(data, "repository/pullRequest/reviewThreads/pageInfo/endCursor", "cursor-1")
		}},
		{"end repeats older", "repeated_cursor", "page_info.endCursor", 3, func(data map[string]any) {
			setDiagnosticFixture(data, "repository/pullRequest/reviewThreads/pageInfo/endCursor", "cursor-1")
		}},
		{"final count mismatch", "count_mismatch", "review_threads.totalCount", 2, func(data map[string]any) {
			setDiagnosticFixture(data, "repository/pullRequest/reviewThreads/pageInfo/hasNextPage", false)
		}},
		{"empty continuation", "empty_continuation", "page_info", 2, func(data map[string]any) {
			setDiagnosticFixture(data, "repository/pullRequest/reviewThreads/nodes", []any{})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client, err := githubapi.NewAPIClient(httpDoerFunc(func(*http.Request) (*http.Response, error) {
				calls++
				if calls > test.pages {
					t.Fatal("pagination did not stop")
				}
				cursor := fmt.Sprintf("cursor-%d", calls)
				data := reviewThreadsGraphQLFixture(3, graphQLPageFixture(true, calls > 1, cursor, cursor), []any{
					reviewThreadGraphQLFixture(fmt.Sprintf("PRRT_%d", calls), false, false, 1, graphQLPageFixture(false, false, "comment", "comment"), []any{
						reviewCommentGraphQLFixture(fmt.Sprintf("PRRC_%d", calls), int64(400+calls), "reviewer", "private-content-sentinel", false, false),
					}),
				})
				requestID, status := diagnosticRequestID, 201
				if calls == test.pages {
					test.mutate(data)
					requestID, status = lastID, 202
				}
				body, _ := json.Marshal(map[string]any{"data": data})
				return &http.Response{StatusCode: status, Header: http.Header{"X-Github-Request-Id": []string{requestID}}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			}), "https://github.test")
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.ListReviewThreads(context.Background(), "credential-sentinel", "acme", "widgets", 23)
			assertDiagnostic(t, err, test.reason, test.field, 202, lastID)
			if calls != test.pages {
				t.Fatalf("calls=%d want %d", calls, test.pages)
			}
		})
	}
}

func TestEmptyPageCursorDiagnostics(t *testing.T) {
	for _, test := range []struct {
		field  string
		value  any
		reason string
	}{
		{"startCursor", "cursor", "empty_page_has_cursor"},
		{"endCursor", "cursor", "empty_page_has_cursor"},
		{"hasNextPage", true, "empty_page_has_next"},
	} {
		t.Run(test.field, func(t *testing.T) {
			page := graphQLPageFixture(false, false, "", "")
			page[test.field] = test.value
			data := reviewThreadsGraphQLFixture(0, page, []any{})
			client, err := githubapi.NewAPIClient(httpDoerFunc(func(*http.Request) (*http.Response, error) {
				body, _ := json.Marshal(map[string]any{"data": data})
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			}), "https://github.test")
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.ListReviewThreads(context.Background(), "credential-sentinel", "acme", "widgets", 23)
			assertDiagnostic(t, err, test.reason, "page_info."+test.field, 200, "")
		})
	}
}

func TestResponseMetadataRedactsCredentialEvenWhenItMatchesRequestIDFormat(t *testing.T) {
	client, err := githubapi.NewAPIClient(httpDoerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"X-Github-Request-Id": []string{diagnosticRequestID}}, Body: io.NopCloser(strings.NewReader(`{"data":null}`))}, nil
	}), "https://github.test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ListReviewThreads(context.Background(), diagnosticRequestID, "acme", "widgets", 23)
	if !errors.Is(err, githubapi.ErrGraphQLQueryFailed) {
		t.Fatalf("sentinel changed: %v", err)
	}
	detail := githubapi.SafeFailureDiagnostics(err)
	if detail.RequestID != "" || detail.HTTPStatus != 200 {
		t.Fatalf("credential exposed as metadata: %+v", detail)
	}
	var staged *githubapi.ReviewThreadReadError
	var graphql *githubapi.GraphQLQueryError
	if !errors.As(err, &staged) || !errors.As(err, &graphql) || staged.RequestID != "" || graphql.RequestID != "" {
		t.Fatal("credential exposed in typed error metadata")
	}
}
