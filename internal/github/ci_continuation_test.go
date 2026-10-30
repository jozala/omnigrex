package github_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCIContinuationRejectsChangedOutputAndAttempt(t *testing.T) {
	generation := &atomic.Int64{}
	client, done := ciClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widgets/check-runs/501":
			fmt.Fprintf(w, `{"id":501,"node_id":"CR_501","name":"build","head_sha":%q,"status":"completed","conclusion":"failure","html_url":"https://github.test/run","completed_at":"2026-10-06T10:00:00Z","output":{"summary":%q}}`, ciHeadSHA, strings.Repeat(fmt.Sprint(generation.Load()), 10000))
		case "/repos/acme/widgets/check-runs/501/annotations":
			fmt.Fprint(w, `[]`)
		case "/repos/acme/widgets/actions/runs/601":
			fmt.Fprintf(w, `{"id":601,"head_sha":%q,"status":"queued","html_url":"https://github.test/run","run_number":1,"run_attempt":%d}`, ciHeadSHA, generation.Load()+1)
		case "/repos/acme/widgets/actions/runs/601/jobs":
			fmt.Fprint(w, `{"total_count":21,"jobs":[`)
			for i := 0; i < 21; i++ {
				if i != 0 {
					fmt.Fprint(w, ",")
				}
				fmt.Fprintf(w, `{"id":%d,"run_id":601,"run_attempt":1,"head_sha":%q,"name":"build","status":"queued","html_url":"https://github.test/job"}`, 701+i, ciHeadSHA)
			}
			fmt.Fprint(w, `]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer done()
	ctx := context.Background()
	first, err := client.GetCheckRunDiagnostics(ctx, "token", "acme", "widgets", 501, "", 9123, ciHeadSHA)
	if err != nil || !first.HasMore {
		t.Fatalf("first=%+v %v", first, err)
	}
	generation.Store(1)
	if _, err := client.GetCheckRunDiagnostics(ctx, "token", "acme", "widgets", 501, first.NextCursor, 9123, ciHeadSHA); err == nil {
		t.Fatal("changed check-output snapshot accepted")
	}
	generation.Store(0)
	detail, err := client.GetWorkflowRunDetail(ctx, "token", "acme", "widgets", 601, 0, "", 9123, ciHeadSHA)
	if err != nil || !detail.HasMore {
		t.Fatalf("detail=%+v %v", detail, err)
	}
	generation.Store(1)
	if _, err := client.GetWorkflowRunDetail(ctx, "token", "acme", "widgets", 601, 0, detail.NextCursor, 9123, ciHeadSHA); err == nil {
		t.Fatal("changed implicit latest attempt accepted")
	}
}
