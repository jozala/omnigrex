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
	// Derive the redirect target from the request host rather than closing
	// over the server variable: the handler runs on server goroutines while
	// the test goroutine assigns that variable, which the race detector
	// reports as a data race.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/logs"):
			w.Header().Set("Location", "http://"+r.Host+"/log-bytes")
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
	apiServer := httptest.NewServer(handler)
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
	first, err := client.GetCIJobLogExcerpt(context.Background(), "token", "acme", "widgets", scope, "", 4096, 9123)
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
		excerpt, err := client.GetCIJobLogExcerpt(context.Background(), "token", "acme", "widgets", scope, cursor, 16384, 9123)
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
	if _, err := client.GetCIJobLogExcerpt(context.Background(), "token", "acme", "widgets", scope, "bad", 4096, 9123); err == nil {
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

func TestSearchBeyondMatchCapPaginatesWithoutLoss(t *testing.T) {
	// 25 consecutive matching lines with context 2: the 21st query line
	// falls inside the 20th match's context window, so resuming from the
	// 20th match's end would skip it. The search must paginate instead of
	// reporting a complete result.
	var builder strings.Builder
	lineOffsets := make([]int64, 0, 25)
	var total int64
	for i := 1; i <= 25; i++ {
		lineOffsets = append(lineOffsets, total)
		line := fmt.Sprintf("match line %d NEEDLE trailing filler text\n", i)
		builder.WriteString(line)
		total += int64(len(line))
	}
	client, done := ciLogServer(t, builder.String(), false)
	defer done()
	scope := githubapi.JobScope{ID: 777, RunID: 37244848238, RunAttempt: 1, HeadSHA: ciHeadSHA}
	first, err := client.SearchCIJobLogs(context.Background(), "token", "acme", "widgets", scope, "NEEDLE", 2, "", 9123)
	if err != nil {
		t.Fatalf("first search error = %v", err)
	}
	if len(first.Matches) != 20 || !first.HasMore || first.NextCursor == "" || first.ReachedEnd {
		t.Fatalf("first page = %d matches has_more=%v reached_end=%v cursor=%q", len(first.Matches), first.HasMore, first.ReachedEnd, first.NextCursor)
	}
	second, err := client.SearchCIJobLogs(context.Background(), "token", "acme", "widgets", scope, "NEEDLE", 2, first.NextCursor, 9123)
	if err != nil {
		t.Fatalf("second search error = %v", err)
	}
	if len(second.Matches) != 5 || second.HasMore || !second.ReachedEnd {
		t.Fatalf("second page = %d matches has_more=%v reached_end=%v", len(second.Matches), second.HasMore, second.ReachedEnd)
	}
	// The continuation must resume at the held-back 21st match's query line,
	// which lies inside the 20th match's context window; resuming from the
	// 20th match's context end would skip lines 21-22. The resumed window
	// starts at line 21, so the re-found match's context is clamped there.
	if got := second.Matches[0].StartOffset; got != lineOffsets[20] {
		t.Fatalf("continuation resumed at offset %d, want 21st query line at %d", got, lineOffsets[20])
	}
	if text := second.Matches[0].Text; !strings.Contains(text, "match line 21 NEEDLE") ||
		strings.Contains(text, "match line 19 NEEDLE") || strings.Contains(text, "match line 20 NEEDLE") {
		t.Fatalf("resumed match does not start at line 21: %q", text)
	}
	if last := second.Matches[4]; last.EndOffset != total {
		t.Fatalf("last match ends at offset %d, want %d", last.EndOffset, total)
	}
	encoded, _ := json.Marshal(first)
	if len(encoded) > 64<<10 {
		t.Fatalf("search response exceeds 64KiB: %d", len(encoded))
	}
}

func TestSearchLongLineExcerptIncludesMatch(t *testing.T) {
	logContent := strings.Repeat("f", 20<<10) + "NEEDLE\nsecond line without the query\n"
	client, done := ciLogServer(t, logContent, false)
	defer done()
	scope := githubapi.JobScope{ID: 777, RunID: 37244848238, RunAttempt: 1, HeadSHA: ciHeadSHA}
	result, err := client.SearchCIJobLogs(context.Background(), "token", "acme", "widgets", scope, "NEEDLE", 0, "", 9123)
	if err != nil {
		t.Fatalf("search error = %v", err)
	}
	if len(result.Matches) != 1 {
		t.Fatalf("matches = %d, want exactly the long-line hit", len(result.Matches))
	}
	match := result.Matches[0]
	if !strings.Contains(match.Text, "NEEDLE") {
		t.Fatalf("excerpt omits the matched text: %q...", match.Text[:min(64, len(match.Text))])
	}
	if !match.TruncatedLine {
		t.Fatal("long-line excerpt must signal truncation")
	}
	if match.StartOffset != 0 || match.EndOffset != int64(20<<10+len("NEEDLE\n")) {
		t.Fatalf("offsets do not locate the source line: %+v", match)
	}
}

func TestSearchLargeLeadingContextShrinksWithinBudget(t *testing.T) {
	// A single match with oversized leading context must shrink from both
	// sides until it fits instead of failing wholesale: trailing lines
	// first, then leading lines, always preserving the query line.
	var builder strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&builder, "pad-%02d-%s\n", i, strings.Repeat("p", 9180))
	}
	builder.WriteString("QUERY-MARKER unique assertion text\n")
	builder.WriteString("after one\nafter two\n")
	client, done := ciLogServer(t, builder.String(), false)
	defer done()
	scope := githubapi.JobScope{ID: 777, RunID: 37244848238, RunAttempt: 1, HeadSHA: ciHeadSHA}
	result, err := client.SearchCIJobLogs(context.Background(), "token", "acme", "widgets", scope, "QUERY-MARKER", 20, "", 9123)
	if err != nil {
		t.Fatalf("oversized-context search error = %v", err)
	}
	if len(result.Matches) != 1 {
		t.Fatalf("matches = %d, want the single query hit", len(result.Matches))
	}
	match := result.Matches[0]
	if !strings.Contains(match.Text, "QUERY-MARKER unique assertion text") {
		t.Fatalf("shrunk excerpt omits the query line")
	}
	if match.StartLine <= 1 || !match.TruncatedLine {
		t.Fatalf("leading context must shrink with truncation flagged: %+v", match)
	}
	if result.HasMore || !result.ReachedEnd {
		t.Fatalf("single complete match must finish: has_more=%v reached_end=%v", result.HasMore, result.ReachedEnd)
	}
	encoded, _ := json.Marshal(result)
	if len(encoded) > 64<<10 {
		t.Fatalf("shrunk response exceeds 64KiB: %d", len(encoded))
	}
}

func TestSearchLiteralAcrossScanBoundary(t *testing.T) {
	// The literal straddles the 8 MiB scan boundary inside one long
	// newline-free region: neither window alone contains it unless the
	// continuation overlaps, and the search must not report completion.
	logContent := strings.Repeat("x", githubapi.MaxCILogScanBytes-3) + "NEEDLE\n" + strings.Repeat("tail line\n", 10)
	client, done := ciLogServer(t, logContent, false)
	defer done()
	scope := githubapi.JobScope{ID: 777, RunID: 37244848238, RunAttempt: 1, HeadSHA: ciHeadSHA}
	first, err := client.SearchCIJobLogs(context.Background(), "token", "acme", "widgets", scope, "NEEDLE", 0, "", 9123)
	if err != nil {
		t.Fatalf("first search error = %v", err)
	}
	if len(first.Matches) != 0 || !first.HasMore || first.NextCursor == "" || first.ReachedEnd {
		t.Fatalf("first page = %d matches has_more=%v reached_end=%v cursor=%q", len(first.Matches), first.HasMore, first.ReachedEnd, first.NextCursor)
	}
	second, err := client.SearchCIJobLogs(context.Background(), "token", "acme", "widgets", scope, "NEEDLE", 0, first.NextCursor, 9123)
	if err != nil {
		t.Fatalf("second search error = %v", err)
	}
	if len(second.Matches) != 1 || second.HasMore || !second.ReachedEnd {
		t.Fatalf("second page = %d matches has_more=%v reached_end=%v", len(second.Matches), second.HasMore, second.ReachedEnd)
	}
	match := second.Matches[0]
	if !strings.Contains(match.Text, "NEEDLE") {
		t.Fatalf("straddling literal not found: %q", match.Text)
	}
	const boundary = int64(githubapi.MaxCILogScanBytes - (githubapi.MaxCIQueryLength - 1))
	if match.StartOffset != boundary {
		t.Fatalf("resumed match starts at offset %d, want scan overlap at %d", match.StartOffset, boundary)
	}
}

func TestCheckOutputContinuationRetrievesTails(t *testing.T) {
	summary := strings.Repeat("s", 10<<10)
	text := strings.Repeat("t", 20<<10)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widgets/check-runs/701" {
			encodedSummary, _ := json.Marshal(summary)
			encodedText, _ := json.Marshal(text)
			fmt.Fprintf(w, `{"id":701,"node_id":"CR_701","name":"CI","head_sha":%q,"status":"completed","conclusion":"failure","html_url":"https://github.test/x","completed_at":"2026-10-06T10:00:00Z","output":{"title":"T","summary":%s,"text":%s}}`, ciHeadSHA, encodedSummary, encodedText)
			return
		}
		if r.URL.Path == "/repos/acme/widgets/check-runs/701/annotations" {
			fmt.Fprint(w, `[{"path":"a.go","start_line":1,"end_line":1,"annotation_level":"failure","message":"first"},{"path":"b.go","start_line":2,"end_line":2,"annotation_level":"warning","message":"second"}]`)
			return
		}
		http.NotFound(w, r)
	})
	client, done := ciClient(t, handler)
	defer done()
	first, err := client.GetCheckRunDiagnostics(context.Background(), "token", "acme", "widgets", 701, "", 9123, ciHeadSHA)
	if err != nil {
		t.Fatalf("first page error = %v", err)
	}
	if first.Output.Summary != summary[:8<<10] || first.Output.Text != text[:16<<10] || len(first.Annotations) != 2 {
		t.Fatalf("first page chunks wrong: summary %d text %d annotations %d", len(first.Output.Summary), len(first.Output.Text), len(first.Annotations))
	}
	if !first.HasMore || first.NextCursor == "" || !first.Truncated {
		t.Fatalf("oversized output must paginate: has_more=%v cursor=%q", first.HasMore, first.NextCursor)
	}
	second, err := client.GetCheckRunDiagnostics(context.Background(), "token", "acme", "widgets", 701, first.NextCursor, 9123, ciHeadSHA)
	if err != nil {
		t.Fatalf("second page error = %v", err)
	}
	if second.Output.Summary != summary[8<<10:] || second.Output.Text != text[16<<10:] {
		t.Fatalf("second page tails wrong: summary %d text %d", len(second.Output.Summary), len(second.Output.Text))
	}
	if second.HasMore || second.NextCursor != "" {
		t.Fatalf("output tails must complete: has_more=%v cursor=%q", second.HasMore, second.NextCursor)
	}
}

func TestCheckAnnotationMessageTailRetrievable(t *testing.T) {
	message := strings.Repeat("m", 4500) + "ASSERTION-AFTER-4K" + strings.Repeat("n", 500)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widgets/check-runs/702" {
			fmt.Fprintf(w, `{"id":702,"node_id":"CR_702","name":"CI","head_sha":%q,"status":"completed","conclusion":"failure","html_url":"https://github.test/x","completed_at":"2026-10-06T10:00:00Z","output":{"title":"T","summary":"S","text":"X"}}`, ciHeadSHA)
			return
		}
		if r.URL.Path == "/repos/acme/widgets/check-runs/702/annotations" {
			encoded, _ := json.Marshal(message)
			fmt.Fprintf(w, `[{"path":"a.go","start_line":1,"end_line":1,"annotation_level":"failure","message":%s}]`, encoded)
			return
		}
		http.NotFound(w, r)
	})
	client, done := ciClient(t, handler)
	defer done()
	first, err := client.GetCheckRunDiagnostics(context.Background(), "token", "acme", "widgets", 702, "", 9123, ciHeadSHA)
	if err != nil {
		t.Fatalf("first page error = %v", err)
	}
	if len(first.Annotations) != 1 || len(first.Annotations[0].Message) != 4<<10 || !first.Annotations[0].Truncated {
		t.Fatalf("first annotation page wrong: %+v", first.Annotations)
	}
	if strings.Contains(first.Annotations[0].Message, "ASSERTION-AFTER-4K") {
		t.Fatal("assertion should sit beyond the first message chunk")
	}
	if !first.HasMore || first.NextCursor == "" || !first.Truncated {
		t.Fatalf("clipped message must paginate with truncation flagged")
	}
	second, err := client.GetCheckRunDiagnostics(context.Background(), "token", "acme", "widgets", 702, first.NextCursor, 9123, ciHeadSHA)
	if err != nil {
		t.Fatalf("second page error = %v", err)
	}
	if len(second.Annotations) != 1 || !strings.Contains(second.Annotations[0].Message, "ASSERTION-AFTER-4K") {
		t.Fatalf("message tail lost: %+v", second.Annotations)
	}
	if second.HasMore {
		t.Fatalf("message tail must complete: has_more=%v", second.HasMore)
	}
}

func TestCheckEmptyOutputReturnsEmptyResult(t *testing.T) {
	// A valid check with empty output and no annotations must succeed on
	// the first request, not be rejected as a stale continuation.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widgets/check-runs/704" {
			fmt.Fprintf(w, `{"id":704,"node_id":"CR_704","name":"CI","head_sha":%q,"status":"completed","conclusion":"success","html_url":"https://github.test/x","completed_at":"2026-10-06T10:00:00Z","output":{}}`, ciHeadSHA)
			return
		}
		if r.URL.Path == "/repos/acme/widgets/check-runs/704/annotations" {
			fmt.Fprint(w, `[]`)
			return
		}
		http.NotFound(w, r)
	})
	client, done := ciClient(t, handler)
	defer done()
	diagnostics, err := client.GetCheckRunDiagnostics(context.Background(), "token", "acme", "widgets", 704, "", 9123, ciHeadSHA)
	if err != nil {
		t.Fatalf("empty diagnostics error = %v", err)
	}
	if diagnostics.CheckID != 704 || diagnostics.Output.Summary != "" || diagnostics.Output.Text != "" || len(diagnostics.Annotations) != 0 {
		t.Fatalf("empty diagnostics = %#v", diagnostics)
	}
	if diagnostics.HasMore || diagnostics.NextCursor != "" || diagnostics.Truncated {
		t.Fatalf("empty diagnostics must complete plainly: has_more=%v cursor=%q truncated=%v", diagnostics.HasMore, diagnostics.NextCursor, diagnostics.Truncated)
	}
}

func TestCheckAnnotationsPaginateWithinBudget(t *testing.T) {
	var annotations []string
	for i := 1; i <= 20; i++ {
		body := fmt.Sprintf("annotation-%02d:", i) + strings.Repeat("m", 4000)
		encoded, _ := json.Marshal(body)
		annotations = append(annotations, fmt.Sprintf(`{"path":"a.go","start_line":%d,"end_line":%d,"annotation_level":"failure","message":%s}`, i, i, encoded))
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widgets/check-runs/703" {
			fmt.Fprintf(w, `{"id":703,"node_id":"CR_703","name":"CI","head_sha":%q,"status":"completed","conclusion":"failure","html_url":"https://github.test/x","completed_at":"2026-10-06T10:00:00Z","output":{"title":"T","summary":"S","text":"X"}}`, ciHeadSHA)
			return
		}
		if r.URL.Path == "/repos/acme/widgets/check-runs/703/annotations" {
			fmt.Fprintf(w, `[%s]`, strings.Join(annotations, ","))
			return
		}
		http.NotFound(w, r)
	})
	client, done := ciClient(t, handler)
	defer done()
	first, err := client.GetCheckRunDiagnostics(context.Background(), "token", "acme", "widgets", 703, "", 9123, ciHeadSHA)
	if err != nil {
		t.Fatalf("first page error = %v", err)
	}
	if len(first.Annotations) == 0 || len(first.Annotations) >= 20 || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("oversized annotations must paginate, got %d annotations has_more=%v", len(first.Annotations), first.HasMore)
	}
	seen := make(map[string]bool)
	for _, annotation := range first.Annotations {
		seen[annotation.Message] = true
	}
	cursor := first.NextCursor
	for cursor != "" {
		page, err := client.GetCheckRunDiagnostics(context.Background(), "token", "acme", "widgets", 703, cursor, 9123, ciHeadSHA)
		if err != nil {
			t.Fatalf("continuation error = %v", err)
		}
		for _, annotation := range page.Annotations {
			if seen[annotation.Message] {
				t.Fatalf("duplicate annotation across pages: %q", annotation.Message[:32])
			}
			seen[annotation.Message] = true
		}
		cursor = page.NextCursor
		if !page.HasMore {
			break
		}
	}
	if len(seen) != 20 {
		t.Fatalf("annotation coverage = %d/20", len(seen))
	}
}

func TestSearchResultsPaginateWithinBudget(t *testing.T) {
	var builder strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&builder, "match-%02d-%s\n", i, strings.Repeat("x", 590))
	}
	client, done := ciLogServer(t, builder.String(), false)
	defer done()
	scope := githubapi.JobScope{ID: 777, RunID: 37244848238, RunAttempt: 1, HeadSHA: ciHeadSHA}
	first, err := client.SearchCIJobLogs(context.Background(), "token", "acme", "widgets", scope, "match-", 5, "", 9123)
	if err != nil {
		t.Fatalf("first search error = %v", err)
	}
	if len(first.Matches) == 0 || len(first.Matches) >= 20 || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("oversized results must paginate, got %d matches has_more=%v", len(first.Matches), first.HasMore)
	}
	// Account exact query lines, not excerpt text: context neighbors also
	// contain the query, so only the match's own line (identified by its
	// position within the excerpt) proves no loss and no duplication.
	queryLine := func(match githubapi.CILogMatch) string {
		segments := strings.Split(match.Text, "\n")
		position := match.LineNumber - match.StartLine
		if position < 0 || position >= len(segments) {
			return ""
		}
		return segments[position]
	}
	seen := make(map[string]bool)
	for _, match := range first.Matches {
		seen[queryLine(match)] = true
	}
	cursor := first.NextCursor
	for cursor != "" {
		page, err := client.SearchCIJobLogs(context.Background(), "token", "acme", "widgets", scope, "match-", 5, cursor, 9123)
		if err != nil {
			t.Fatalf("continuation error = %v", err)
		}
		for _, match := range page.Matches {
			line := queryLine(match)
			if seen[line] {
				t.Fatalf("duplicate query line across pages: %q", line[:32])
			}
			seen[line] = true
		}
		cursor = page.NextCursor
		if !page.HasMore {
			break
		}
	}
	if len(seen) != 20 {
		t.Fatalf("query-line coverage = %d/20", len(seen))
	}
	for i := 1; i <= 20; i++ {
		want := fmt.Sprintf("match-%02d-%s", i, strings.Repeat("x", 590))
		if !seen[want] {
			t.Fatalf("query line %d lost across pagination", i)
		}
	}
}

func TestSearchOverlapReportsMatchExactlyOnce(t *testing.T) {
	// A giant newline-free line carries one occurrence fully inside the
	// prior window but within its trailing overlap zone, and the line
	// extends past the window end: the occurrence must be deferred, then
	// reported exactly once on the next page.
	logContent := strings.Repeat("y", githubapi.MaxCILogScanBytes-200) + "NEEDLE" + strings.Repeat("y", 10000) + "\n" + strings.Repeat("tail line\n", 10)
	client, done := ciLogServer(t, logContent, false)
	defer done()
	scope := githubapi.JobScope{ID: 777, RunID: 37244848238, RunAttempt: 1, HeadSHA: ciHeadSHA}
	first, err := client.SearchCIJobLogs(context.Background(), "token", "acme", "widgets", scope, "NEEDLE", 2, "", 9123)
	if err != nil {
		t.Fatalf("first search error = %v", err)
	}
	if len(first.Matches) != 0 || !first.HasMore || first.NextCursor == "" || first.ReachedEnd {
		t.Fatalf("overlapped match must defer: %d matches has_more=%v reached_end=%v cursor=%q", len(first.Matches), first.HasMore, first.ReachedEnd, first.NextCursor)
	}
	second, err := client.SearchCIJobLogs(context.Background(), "token", "acme", "widgets", scope, "NEEDLE", 2, first.NextCursor, 9123)
	if err != nil {
		t.Fatalf("second search error = %v", err)
	}
	if len(second.Matches) != 1 || second.HasMore || !second.ReachedEnd {
		t.Fatalf("second page = %d matches has_more=%v reached_end=%v", len(second.Matches), second.HasMore, second.ReachedEnd)
	}
	if !strings.Contains(second.Matches[0].Text, "NEEDLE") {
		t.Fatalf("deferred occurrence lost: %q", second.Matches[0].Text)
	}
}

func TestSearchAcrossNewlineBoundaryWithoutDuplicates(t *testing.T) {
	// A query line cut by the 8 MiB window end must be reported exactly
	// once: matching on context reaching back across the newline-anchored
	// resume boundary would duplicate it on the next page.
	// Layout (all lines 4 bytes except the query line): the query line
	// starts just inside window 1 with its occurrence fully scanned, so
	// the old context-based filter keeps it on page 1 and finds it again
	// on page 2.
	const padLines = (8<<20)/4 - 10
	logContent := strings.Repeat("pad\n", padLines) + "NEEDLE" + strings.Repeat("z", 200) + "\n" + strings.Repeat("end\n", 5)
	client, done := ciLogServer(t, logContent, false)
	defer done()
	scope := githubapi.JobScope{ID: 777, RunID: 37244848238, RunAttempt: 1, HeadSHA: ciHeadSHA}
	first, err := client.SearchCIJobLogs(context.Background(), "token", "acme", "widgets", scope, "NEEDLE", 2, "", 9123)
	if err != nil {
		t.Fatalf("first search error = %v", err)
	}
	// The only occurrence reaches past the resume boundary, so page 1 must
	// defer it rather than report it twice.
	if len(first.Matches) != 0 || !first.HasMore || first.NextCursor == "" || first.ReachedEnd {
		t.Fatalf("first page = %d matches has_more=%v reached_end=%v cursor=%q", len(first.Matches), first.HasMore, first.ReachedEnd, first.NextCursor)
	}
	second, err := client.SearchCIJobLogs(context.Background(), "token", "acme", "widgets", scope, "NEEDLE", 2, first.NextCursor, 9123)
	if err != nil {
		t.Fatalf("second search error = %v", err)
	}
	if len(second.Matches) != 1 || second.HasMore || !second.ReachedEnd {
		t.Fatalf("second page = %d matches has_more=%v reached_end=%v", len(second.Matches), second.HasMore, second.ReachedEnd)
	}
	if !strings.Contains(second.Matches[0].Text, "NEEDLE") {
		t.Fatalf("deferred occurrence lost: %q", second.Matches[0].Text)
	}
}

func TestSearchOverflowBandPaginatesWithinBudget(t *testing.T) {
	// Overflow-band configuration: a maximal query with match sizes where
	// the old 256-byte-cursor probe accepted a full page but the finalized
	// result with the real cursor exceeded 64 KiB and was rejected
	// wholesale. The conservative probe must paginate instead.
	query := strings.Repeat("q", githubapi.MaxCIQueryLength)
	var builder strings.Builder
	var want []string
	for i := 1; i <= 21; i++ {
		line := fmt.Sprintf("L%02d:", i) + query + strings.Repeat("y", 2876+(i%9))
		want = append(want, line)
		builder.WriteString(line + "\n")
	}
	client, done := ciLogServer(t, builder.String(), false)
	defer done()
	scope := githubapi.JobScope{ID: 777, RunID: 37244848238, RunAttempt: 1, HeadSHA: ciHeadSHA}
	first, err := client.SearchCIJobLogs(context.Background(), "token", "acme", "widgets", scope, query, 0, "", 9123)
	if err != nil {
		t.Fatalf("first search error = %v", err)
	}
	if !first.HasMore || first.NextCursor == "" {
		t.Fatalf("overflow band must paginate: has_more=%v cursor=%q matches=%d", first.HasMore, first.NextCursor, len(first.Matches))
	}
	seen := make(map[string]int)
	for _, match := range first.Matches {
		seen[match.Text]++
	}
	cursor := first.NextCursor
	for cursor != "" {
		page, err := client.SearchCIJobLogs(context.Background(), "token", "acme", "widgets", scope, query, 0, cursor, 9123)
		if err != nil {
			t.Fatalf("continuation error = %v", err)
		}
		for _, match := range page.Matches {
			seen[match.Text]++
		}
		cursor = page.NextCursor
		if !page.HasMore {
			if page.ReachedEnd != true {
				t.Fatalf("final page must report reached_end")
			}
			break
		}
	}
	if len(seen) != 21 {
		t.Fatalf("query-line coverage = %d/21", len(seen))
	}
	for _, line := range want {
		if seen[line] != 1 {
			t.Fatalf("query line reported %d times, want exactly once", seen[line])
		}
	}
}

func TestLogDownloadRejectsUnsafeDestinations(t *testing.T) {
	for _, location := range []string{
		"http://169.254.169.254/latest",
		"https://user:pass@example.blob.core.windows.net/logs",
		"https://example.blob.core.windows.net:8443/logs",
		"https://evil.example/logs",
		"http://example.blob.core.windows.net/logs",
		"https://evilgithubusercontent.com/logs",
		"https://evilobjects.githubusercontent.com/logs",
		"https://evilactions.githubusercontent.com/logs",
	} {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", location)
			w.WriteHeader(http.StatusFound)
		})
		client, done := ciClient(t, handler)
		scope := githubapi.JobScope{ID: 1, RunID: 1, HeadSHA: ciHeadSHA}
		_, err := client.GetCIJobLogExcerpt(context.Background(), "token", "acme", "widgets", scope, "", 4096, 9123)
		done()
		if err == nil {
			t.Fatalf("unsafe location accepted: %s", location)
		}
		if detail := githubapi.SafeFailureDiagnostics(err); detail.Stage != "ci_logs_download" || detail.Reason != "invalid_url" {
			t.Fatalf("unsafe location %s rejected at wrong stage: %+v", location, detail)
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
		_, err := client.GetCIJobLogExcerpt(context.Background(), "token", "acme", "widgets", scope, "", 4096, 9123)
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
	_, err := client.GetCIJobLogExcerpt(ctx, "token", "acme", "widgets", scope, "", 4096, 9123)
	if err == nil || githubapi.CancellationReason(err) == "" {
		t.Fatalf("cancellation not preserved: %v", err)
	}
}
