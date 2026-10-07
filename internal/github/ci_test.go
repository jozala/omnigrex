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
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
)

const ciHeadSHA = "16d6f971a458044c1d6445171d20e0c3ef9b12fd"
const ciOtherSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func ciClient(t *testing.T, handler http.Handler) (*githubapi.APIClient, func()) {
	t.Helper()
	server := httptest.NewServer(handler)
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return client, server.Close
}

func TestGetCheckRunDiagnosticsSuccessAndScopeRejection(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/widgets/check-runs/501":
			fmt.Fprintf(w, `{"id":501,"node_id":"CR_501","name":"CI","head_sha":%q,"status":"completed","conclusion":"failure","html_url":"https://github.test/acme/widgets/runs/501","details_url":"https://github.test/acme/widgets/runs/501","completed_at":"2026-10-06T10:00:00Z","output":{"title":"Build failed","summary":"2 failures","text":"assertion failed"}}`, ciHeadSHA)
		case r.URL.Path == "/repos/acme/widgets/check-runs/501/annotations":
			fmt.Fprint(w, `[{"path":"internal/worker.go","start_line":10,"end_line":10,"annotation_level":"failure","message":"assertion failed","title":"Failure"}]`)
		case r.URL.Path == "/repos/acme/widgets/check-runs/502":
			fmt.Fprintf(w, `{"id":502,"node_id":"CR_502","name":"CI","head_sha":%q,"status":"completed","conclusion":"failure","html_url":"https://github.test/acme/widgets/runs/502","completed_at":"2026-10-06T10:00:00Z","output":{}}`, ciOtherSHA)
		case r.URL.Path == "/repos/acme/widgets/check-runs/502/annotations":
			fmt.Fprint(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	})
	client, done := ciClient(t, handler)
	defer done()
	diagnostics, err := client.GetCheckRunDiagnostics(context.Background(), "token", "acme", "widgets", 501, "", 9123, ciHeadSHA)
	if err != nil {
		t.Fatalf("GetCheckRunDiagnostics() error = %v detail=%+v", err, githubapi.SafeFailureDiagnostics(err))
	}
	if diagnostics.CheckID != 501 || diagnostics.HeadSHA != ciHeadSHA || diagnostics.Output.Summary != "2 failures" || len(diagnostics.Annotations) != 1 {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	if _, err := client.GetCheckRunDiagnostics(context.Background(), "token", "acme", "widgets", 502, "", 9123, ciHeadSHA); err == nil || !errors.Is(err, githubapi.ErrInvalidAPIResponse) {
		t.Fatalf("cross-head check error = %v", err)
	}
	if _, err := client.GetCheckRunDiagnostics(context.Background(), "token", "acme", "widgets", 999, "", 9123, ciHeadSHA); err == nil {
		t.Fatal("missing check error = nil")
	} else if detail := githubapi.SafeFailureDiagnostics(err); detail.HTTPStatus != 404 {
		t.Fatalf("missing check status = %+v", detail)
	}
}

func TestCheckDiagnosticsBoundsAndContinuation(t *testing.T) {
	largeSummary := strings.Repeat("s", 10<<10)
	largeText := strings.Repeat("t", 20<<10)
	var annotations []string
	for i := 1; i <= 25; i++ {
		annotations = append(annotations, fmt.Sprintf(`{"path":"a.go","start_line":%d,"end_line":%d,"annotation_level":"failure","message":%q}`, i, i, strings.Repeat("m", 100)))
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widgets/check-runs/601" {
			summary, _ := json.Marshal(largeSummary)
			text, _ := json.Marshal(largeText)
			fmt.Fprintf(w, `{"id":601,"node_id":"CR_601","name":"CI","head_sha":%q,"status":"completed","conclusion":"failure","html_url":"https://github.test/x","completed_at":"2026-10-06T10:00:00Z","output":{"title":"T","summary":%s,"text":%s}}`, ciHeadSHA, summary, text)
			return
		}
		if r.URL.Path == "/repos/acme/widgets/check-runs/601/annotations" {
			fmt.Fprintf(w, `[%s]`, strings.Join(annotations, ","))
			return
		}
		http.NotFound(w, r)
	})
	client, done := ciClient(t, handler)
	defer done()
	first, err := client.GetCheckRunDiagnostics(context.Background(), "token", "acme", "widgets", 601, "", 9123, ciHeadSHA)
	if err != nil {
		t.Fatalf("first page error = %v", err)
	}
	if !first.HasMore || first.NextCursor == "" || !first.Truncated {
		t.Fatalf("expected continuation, got %#v", first)
	}
	if len(first.Annotations) != 20 || len(first.Output.Summary) != 8<<10 || len(first.Output.Text) != 16<<10 {
		t.Fatalf("bounds = summary %d text %d annotations %d", len(first.Output.Summary), len(first.Output.Text), len(first.Annotations))
	}
	second, err := client.GetCheckRunDiagnostics(context.Background(), "token", "acme", "widgets", 601, first.NextCursor, 9123, ciHeadSHA)
	if err != nil {
		t.Fatalf("second page error = %v", err)
	}
	if second.HasMore || len(second.Annotations) != 5 {
		t.Fatalf("second page = %#v", second)
	}
	if _, err := client.GetCheckRunDiagnostics(context.Background(), "token", "acme", "widgets", 601, first.NextCursor, 9999, ciHeadSHA); err == nil {
		t.Fatal("forged continuation repo accepted")
	}
	if _, err := client.GetCheckRunDiagnostics(context.Background(), "token", "acme", "widgets", 602, first.NextCursor, 9123, ciHeadSHA); err == nil {
		t.Fatal("continuation for other check accepted")
	}
	if _, err := client.GetCheckRunDiagnostics(context.Background(), "token", "acme", "widgets", 601, "not-a-cursor", 9123, ciHeadSHA); err == nil {
		t.Fatal("invalid cursor accepted")
	}
	encoded, _ := json.Marshal(first)
	if len(encoded) > 64<<10 {
		t.Fatalf("response exceeds 64KiB: %d", len(encoded))
	}
}

func TestListAndGetRunWithJobsAndMergeRef(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/widgets/actions/runs" && r.URL.Query().Get("head_sha") == ciHeadSHA:
			fmt.Fprintf(w, `{"total_count":1,"workflow_runs":[{"id":37244848238,"name":"CI","head_branch":"main","head_sha":%q,"status":"completed","conclusion":"failure","html_url":"https://github.test/runs/37244848238","run_number":42,"run_attempt":2}]}`, ciHeadSHA)
		case r.URL.Path == "/repos/acme/widgets/actions/runs/37244848238":
			fmt.Fprintf(w, `{"id":37244848238,"name":"CI","head_branch":"main","head_sha":%q,"status":"completed","conclusion":"failure","html_url":"https://github.test/runs/37244848238","run_number":42,"run_attempt":2}`, ciHeadSHA)
		case r.URL.Path == "/repos/acme/widgets/actions/runs/37244848238/jobs":
			fmt.Fprintf(w, `{"total_count":1,"jobs":[{"id":111560587662,"run_id":37244848238,"run_attempt":2,"head_sha":%q,"node_id":"J_1","name":"test","status":"completed","conclusion":"failure","html_url":"https://github.test/jobs/111560587662","started_at":"2026-10-06T10:00:00Z","completed_at":"2026-10-06T10:05:00Z","steps":[{"name":"Set up","number":1,"status":"completed","conclusion":"success"},{"name":"Run Docker-backed integration tests","number":13,"status":"completed","conclusion":"failure"}]}]}`, ciHeadSHA)
		case r.URL.Path == "/repos/acme/widgets/actions/runs/37244848238/attempts/2/jobs":
			fmt.Fprintf(w, `{"total_count":1,"jobs":[{"id":111560587662,"run_id":37244848238,"run_attempt":2,"head_sha":%q,"node_id":"J_1","name":"test","status":"completed","conclusion":"failure","html_url":"https://github.test/jobs/111560587662","started_at":"2026-10-06T10:00:00Z","completed_at":"2026-10-06T10:05:00Z","steps":[{"name":"Run Docker-backed integration tests","number":13,"status":"completed","conclusion":"failure"}]}]}`, ciHeadSHA)
		case r.URL.Path == "/repos/acme/widgets/actions/runs/999":
			fmt.Fprintf(w, `{"id":999,"head_sha":%q,"status":"completed","conclusion":"failure","html_url":"https://github.test/runs/999","run_number":1}`, ciOtherSHA)
		case r.URL.Path == "/repos/acme/widgets/actions/runs/999/jobs":
			fmt.Fprintf(w, `{"total_count":1,"jobs":[{"id":1000,"run_id":999,"run_attempt":1,"head_sha":%q,"node_id":"J_2","name":"merge","status":"completed","conclusion":"failure","html_url":"https://github.test/jobs/1000","started_at":"2026-10-06T10:00:00Z","completed_at":"2026-10-06T10:05:00Z","steps":[]}]}`, ciOtherSHA)
		default:
			http.NotFound(w, r)
		}
	})
	client, done := ciClient(t, handler)
	defer done()
	list, err := client.ListWorkflowRunsForHead(context.Background(), "token", "acme", "widgets", ciHeadSHA, "", 9123)
	if err != nil || len(list.Runs) != 1 || list.Runs[0].ID != 37244848238 || list.Runs[0].RunAttempt != 2 {
		t.Fatalf("list runs = %#v, %v", list, err)
	}
	detail, err := client.GetWorkflowRunDetail(context.Background(), "token", "acme", "widgets", 37244848238, 2, "", 9123, ciHeadSHA)
	if err != nil || len(detail.Jobs) != 1 || detail.Jobs[0].Steps[0].Number != 13 {
		t.Fatalf("run detail = %#v, %v", detail, err)
	}
	if _, err := client.GetWorkflowRunDetail(context.Background(), "token", "acme", "widgets", 999, 0, "", 9123, ciHeadSHA); err == nil || !errors.Is(err, githubapi.ErrInvalidAPIResponse) {
		t.Fatalf("merge-ref run error = %v", err)
	}
}

func ciLogServer(t *testing.T, logContent string, supportRange bool) (*githubapi.APIClient, func()) {
	t.Helper()
	var apiServer *httptest.Server
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/logs"):
			w.Header().Set("Location", apiServer.URL+"/log-bytes")
			w.WriteHeader(http.StatusFound)
		case r.URL.Path == "/log-bytes":
			if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				http.Error(w, "auth forwarded", http.StatusBadRequest)
				return
			}
			content := logContent
			if rng := r.Header.Get("Range"); rng != "" && supportRange {
				var start int64
				if _, err := fmt.Sscanf(rng, "bytes=%d-", &start); err == nil && start < int64(len(content)) {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(content)-1, len(content)))
					w.WriteHeader(http.StatusPartialContent)
					_, _ = io.WriteString(w, content[start:])
					return
				}
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			_, _ = io.WriteString(w, content)
		case strings.HasPrefix(r.URL.Path, "/repos/acme/widgets/actions/jobs/"):
			parts := strings.Split(r.URL.Path, "/")
			id := parts[len(parts)-1]
			if id == "777" {
				fmt.Fprintf(w, `{"id":777,"run_id":37244848238,"run_attempt":1,"head_sha":%q,"node_id":"J_777","name":"test","status":"completed","conclusion":"failure","html_url":"https://github.test/jobs/777","started_at":"2026-10-06T10:00:00Z","completed_at":"2026-10-06T10:05:00Z","steps":[{"name":"Run tests","number":1,"status":"completed","conclusion":"failure"}]}`, ciHeadSHA)
				return
			}
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, "/repos/acme/widgets/actions/runs/"):
			fmt.Fprintf(w, `{"id":37244848238,"head_sha":%q,"status":"completed","conclusion":"failure","html_url":"https://github.test/runs/37244848238","run_number":1}`, ciHeadSHA)
		default:
			http.NotFound(w, r)
		}
	})
	apiServer = httptest.NewServer(handler)
	client, err := githubapi.NewAPIClient(apiServer.Client(), apiServer.URL)
	if err != nil {
		apiServer.Close()
		t.Fatal(err)
	}
	return client, apiServer.Close
}

func TestJobLogExcerptBoundedContinuationAndLateRetrieval(t *testing.T) {
	prefix := strings.Repeat("setup line\n", 2000)
	assertion := "CloseMutationAdmission() error = agent turn fence lost\n"
	suffix := strings.Repeat("trailing line\n", 500)
	logContent := prefix + assertion + suffix
	client, done := ciLogServer(t, logContent, true)
	defer done()
	scope := githubapi.JobScope{ID: 777, RunID: 37244848238, RunAttempt: 1, HeadSHA: ciHeadSHA}
	first, err := client.GetCIJobLogExcerpt(context.Background(), "token", "acme", "widgets", scope, nil, "", 4096, 9123)
	if err != nil {
		t.Fatalf("first excerpt error = %v", err)
	}
	if !first.HasMore || first.NextCursor == "" || !first.Truncated {
		t.Fatalf("expected continuation, got %#v", first)
	}
	if strings.Contains(first.Text, "agent turn fence lost") {
		t.Fatal("assertion should be beyond first window")
	}
	encoded, _ := json.Marshal(first)
	if len(encoded) > 64<<10 {
		t.Fatalf("excerpt exceeds 64KiB: %d", len(encoded))
	}
	cursor := first.NextCursor
	found := false
	for i := 0; i < 20 && cursor != ""; i++ {
		excerpt, err := client.GetCIJobLogExcerpt(context.Background(), "token", "acme", "widgets", scope, nil, cursor, 16384, 9123)
		if err != nil {
			t.Fatalf("continuation %d error = %v", i, err)
		}
		if strings.Contains(excerpt.Text, "agent turn fence lost") {
			found = true
			break
		}
		cursor = excerpt.NextCursor
		if !excerpt.HasMore {
			break
		}
	}
	if !found {
		t.Fatal("assertion not reachable through continuation")
	}
	if _, err := client.GetCIJobLogExcerpt(context.Background(), "token", "acme", "widgets", scope, nil, "bad", 4096, 9123); err == nil {
		t.Fatal("invalid cursor accepted")
	}
}

func TestSearchPreservesContextAcrossChunksAndLongLines(t *testing.T) {
	longLine := strings.Repeat("x", 20<<10) + " TARGET " + strings.Repeat("y", 20<<10)
	logContent := "line one\nline two TARGET line\n" + longLine + "\nline four TARGET end\n"
	client, done := ciLogServer(t, logContent, false)
	defer done()
	scope := githubapi.JobScope{ID: 777, RunID: 37244848238, RunAttempt: 1, HeadSHA: ciHeadSHA}
	result, err := client.SearchCIJobLogs(context.Background(), "token", "acme", "widgets", scope, "TARGET", 2, "", 9123)
	if err != nil {
		t.Fatalf("search error = %v", err)
	}
	if len(result.Matches) == 0 || result.SearchedBytes == 0 {
		t.Fatalf("no matches: %#v", result)
	}
	for _, match := range result.Matches {
		if !strings.Contains(match.Text, "TARGET") {
			t.Fatalf("match without query: %#v", match)
		}
	}
	encoded, _ := json.Marshal(result)
	if len(encoded) > 64<<10 {
		t.Fatalf("search response exceeds 64KiB: %d", len(encoded))
	}
}

func TestLogDownloadRejectsUnsafeDestinations(t *testing.T) {
	for _, location := range []string{
		"http://169.254.169.254/latest",
		"https://user:pass@example.blob.core.windows.net/logs",
		"https://example.blob.core.windows.net:8443/logs",
		"https://evil.example/logs",
		"http://example.blob.core.windows.net/logs",
	} {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", location)
			w.WriteHeader(http.StatusFound)
		})
		client, done := ciClient(t, handler)
		scope := githubapi.JobScope{ID: 1, RunID: 1, HeadSHA: ciHeadSHA}
		_, err := client.GetCIJobLogExcerpt(context.Background(), "token", "acme", "widgets", scope, nil, "", 4096, 9123)
		done()
		if err == nil {
			t.Fatalf("unsafe location accepted: %s", location)
		}
		if strings.Contains(err.Error(), location) {
			t.Fatalf("signed URL leaked into error for %s", location)
		}
	}
}

func TestLogStatesAreDistinguishable(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/logs") {
			w.Header().Set("X-GitHub-Request-Id", "A41A:2AF0FB:6AAAD0:6A71D9:6ABC2962")
			if strings.Contains(r.URL.Path, "/jobs/404/") {
				http.NotFound(w, r)
				return
			}
			if strings.Contains(r.URL.Path, "/jobs/410/") {
				http.Error(w, `{"message":"logs expired"}`, http.StatusGone)
				return
			}
			if strings.Contains(r.URL.Path, "/jobs/403/") {
				http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)
				return
			}
			if strings.Contains(r.URL.Path, "/jobs/429/") {
				w.Header().Set("Retry-After", "1")
				http.Error(w, `{"message":"rate limited"}`, http.StatusTooManyRequests)
				return
			}
			http.NotFound(w, r)
			return
		}
		http.NotFound(w, r)
	})
	client, done := ciClient(t, handler)
	defer done()
	for _, test := range []struct {
		jobID  int64
		status int
	}{
		{jobID: 404, status: 404},
		{jobID: 410, status: 410},
		{jobID: 403, status: 403},
		{jobID: 429, status: 429},
	} {
		scope := githubapi.JobScope{ID: test.jobID, RunID: 1, HeadSHA: ciHeadSHA}
		_, err := client.GetCIJobLogExcerpt(context.Background(), "token", "acme", "widgets", scope, nil, "", 4096, 9123)
		if err == nil {
			t.Fatalf("job %d error = nil", test.jobID)
		}
		if detail := githubapi.SafeFailureDiagnostics(err); detail.HTTPStatus != test.status {
			t.Fatalf("job %d status = %+v", test.jobID, detail)
		}
		if strings.Contains(err.Error(), "token") {
			t.Fatalf("credential leaked for job %d", test.jobID)
		}
	}
}

func TestLogCancellationIsDistinguishable(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		fmt.Fprint(w, "too late")
	})
	client, done := ciClient(t, handler)
	defer done()
	_ = client
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	scope := githubapi.JobScope{ID: 777, RunID: 1, HeadSHA: ciHeadSHA}
	_, err := client.GetCIJobLogExcerpt(ctx, "token", "acme", "widgets", scope, nil, "", 4096, 9123)
	if err == nil || githubapi.CancellationReason(err) == "" {
		t.Fatalf("cancellation not preserved: %v", err)
	}
}
