package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/workflow"
)

const evidenceMerge = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const evidenceBase = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// The real API client and backend are used, not observer-only mocks.
func evidenceBackend(t *testing.T) (*mcp.ProductionBackend, mcp.ToolScope, *atomic.Int64) {
	t.Helper()
	mode := &atomic.Int64{}
	prReads := &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/logs" && r.Header.Get("Authorization") != "Bearer developer-secret" {
			t.Error("wrong CI credential authority")
			w.WriteHeader(403)
			return
		}
		root := "/repos/acme/widgets"
		switch {
		case r.URL.Path == root+"/pulls/23":
			head := ciRegressionHead
			prID := 654
			repo := 9123
			mergeable := `true`
			merge := evidenceMerge
			reads := prReads.Add(1)
			switch mode.Load() {
			case 1:
				head = evidenceBase
			case 3:
				merge = evidenceBase
			case 4:
				repo = 999
			case 6:
				mergeable = `null`
			case 11:
				prID = 999
			case 13:
				if reads%2 == 0 {
					head = evidenceBase
				}
			}
			fmt.Fprintf(w, `{"id":%d,"node_id":"PR_23","number":23,"title":"Changes","state":"open","html_url":"https://github.test/acme/widgets/pull/23","head":{"sha":%q,"ref":"omnigrex/issue-12"},"base":{"sha":%q,"ref":"main","repo":{"id":%d,"full_name":"acme/widgets"}},"mergeable":%s,"merge_commit_sha":%q}`, prID, head, evidenceBase, repo, mergeable, merge)
		case r.URL.Path == root+"/branches/main":
			base := evidenceBase
			if mode.Load() == 2 {
				base = evidenceMerge
			}
			fmt.Fprintf(w, `{"name":"main","protected":true,"commit":{"sha":%q}}`, base)
		case r.URL.Path == root+"/branches/main/protection":
			fmt.Fprint(w, `{"required_status_checks":{"contexts":["build","missing"],"checks":[{"context":"build","app_id":7},{"context":"missing","app_id":7}]}}`)
		case r.URL.Path == root+"/rules/branches/main":
			fmt.Fprint(w, `[{"type":"workflows","ruleset_id":10,"ruleset_source_type":"Organization","ruleset_source":"acme"}]`)
		case r.URL.Path == root+"/git/commits/"+evidenceMerge || r.URL.Path == root+"/git/commits/"+evidenceBase:
			parent := ciRegressionHead
			if mode.Load() == 5 {
				parent = evidenceMerge
			}
			fmt.Fprintf(w, `{"sha":%q,"parents":[{"sha":%q},{"sha":%q}]}`, evidenceMerge, evidenceBase, parent)
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			head := ciRegressionHead
			if strings.Contains(r.URL.Path, evidenceMerge) {
				head = evidenceMerge
			}
			id := 501
			if head == ciRegressionHead {
				id = 502
			}
			fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"id":%d,"node_id":"CR_501","name":"build","head_sha":%q,"app":{"id":7,"slug":"builder"},"status":"queued","html_url":"https://github.test/check/501"}]}`, id, head)
		case strings.HasSuffix(r.URL.Path, "/statuses"):
			head := ciRegressionHead
			if strings.Contains(r.URL.Path, evidenceMerge) {
				head = evidenceMerge
			}
			id := 1
			if head == evidenceMerge {
				id = 2
			}
			fmt.Fprintf(w, `[{"id":%d,"node_id":"CS_1","context":"build","state":"failure","url":"https://api.github.test/repos/acme/widgets/statuses/%s","created_at":"2026-10-06T10:00:00Z","updated_at":"2026-10-06T10:00:00Z"}]`, id, head)
		case r.URL.Path == root+"/check-runs/501":
			fmt.Fprintf(w, `{"id":501,"node_id":"CR_501","name":"build","head_sha":%q,"app":{"id":7},"status":"queued","html_url":"https://github.test/check/501","output":{"summary":"pending test merge"}}`, evidenceMerge)
		case r.URL.Path == root+"/check-runs/501/annotations":
			fmt.Fprint(w, `[]`)
		case r.URL.Path == root+"/actions/runs":
			if r.URL.Query().Get("head_sha") == ciRegressionHead {
				fmt.Fprint(w, `{"total_count":0,"workflow_runs":[]}`)
				return
			}
			fmt.Fprint(w, `{"total_count":1,"workflow_runs":[`)
			writeEvidenceRun(w, mode.Load())
			fmt.Fprint(w, `]}`)
		case r.URL.Path == root+"/actions/runs/601":
			writeEvidenceRun(w, mode.Load())
		case r.URL.Path == root+"/actions/runs/601/jobs":
			fmt.Fprintf(w, `{"total_count":1,"jobs":[{"id":701,"run_id":601,"run_attempt":1,"head_sha":%q,"name":"build","status":"completed","conclusion":"failure","completed_at":"2026-10-06T10:00:00Z","html_url":"https://github.test/job"}]}`, evidenceMerge)
		case r.URL.Path == root+"/actions/jobs/701":
			// Missing job head must be authorized via its parent, not rewritten to the
			// reviewed head. Mode 8 makes the parent conflict with the job metadata.
			head := ""
			if mode.Load() == 8 {
				head = ciRegressionHead
			}
			fmt.Fprintf(w, `{"id":701,"run_id":601,"run_attempt":1,"head_sha":%q,"name":"build","status":"completed","conclusion":"failure","completed_at":"2026-10-06T10:00:00Z","html_url":"https://github.test/job"}`, head)
		case r.URL.Path == root+"/actions/jobs/701/logs":
			w.Header().Set("Location", "http://"+r.Host+"/logs")
			w.WriteHeader(302)
		case r.URL.Path == "/logs":
			if r.Header.Get("Authorization") != "" {
				t.Error("credential forwarded")
			}
			fmt.Fprint(w, "FAIL: verified merge\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	backend, scope := ciBackend(t, client, workflow.RoleReviewer)
	return backend, scope, mode
}
func writeEvidenceRun(w http.ResponseWriter, mode int64) {
	event := "pull_request"
	repo := 9123
	if mode == 7 {
		event = "merge_group"
	}
	if mode == 9 {
		repo = 999
	}
	head := evidenceMerge
	if mode == 10 {
		head = ciRegressionHead
	}
	fmt.Fprintf(w, `{"id":601,"head_sha":%q,"repository":{"id":%d},"event":%q,"status":"completed","conclusion":"failure","html_url":"https://github.test/run","run_number":1,"run_attempt":1}`, head, repo, event)
}

func TestExistingCIToolsExposePolicyStatusesAndVerifiedTestMerge(t *testing.T) {
	backend, scope, _ := evidenceBackend(t)
	raw := ciExecute(t, backend, scope, mcp.ToolGetCheckRuns, `{}`)
	var evidence githubapi.CIEvidencePage
	if json.Unmarshal(raw, &evidence) != nil || evidence.Mapping.State != "verified" || evidence.Mapping.ReviewedHead != ciRegressionHead || evidence.Mapping.TestMergeSHA != evidenceMerge || len(evidence.Targets) != 2 || len(evidence.Inventory.Unresolved) != 1 {
		t.Fatalf("evidence=%s", raw)
	}
	for _, target := range evidence.Targets {
		if len(target.Observation.Statuses) != 1 || len(target.Required) != 3 || target.Required[0].State != githubapi.CIPending || target.Required[1].State != githubapi.CIUnresolved || target.Required[2].State != githubapi.CIMissing {
			t.Fatalf("target=%+v", target)
		}
	}
	for _, tool := range []struct{ name, args string }{
		{mcp.ToolGetCheckRunDiagnostics, `{"check_run_id":501}`}, {mcp.ToolListCIRuns, `{}`}, {mcp.ToolGetCIRun, `{"run_id":601}`}, {mcp.ToolGetCIJobLogs, `{"job_id":701}`}, {mcp.ToolSearchCIJobLogs, `{"job_id":701,"query":"FAIL"}`},
	} {
		raw := ciExecute(t, backend, scope, tool.name, tool.args)
		if !strings.Contains(string(raw), evidenceMerge) || !strings.Contains(string(raw), `"target_mapping"`) {
			t.Fatalf("%s=%s", tool.name, raw)
		}
	}
	if scope.HeadSHA != ciRegressionHead {
		t.Fatal("review authority changed")
	}
	raw = ciExecute(t, backend, scope, mcp.ToolGetPullRequest, `{}`)
	if strings.Contains(string(raw), `"head":{"label":"","ref":"omnigrex/issue-12","sha":"`+evidenceMerge) {
		t.Fatal("merge target leaked into PR metadata")
	}
}

func TestTestMergeAuthorityInvalidatedAcrossEveryDiagnosticPath(t *testing.T) {
	backend, scope, mode := evidenceBackend(t)
	for _, state := range []int64{1, 2, 3, 4, 5, 6, 11, 13} {
		mode.Store(state)
		for _, tool := range []struct{ name, args string }{
			{mcp.ToolGetCheckRunDiagnostics, `{"check_run_id":501}`}, {mcp.ToolGetCIRun, `{"run_id":601}`}, {mcp.ToolGetCIJobLogs, `{"job_id":701}`}, {mcp.ToolSearchCIJobLogs, `{"job_id":701,"query":"FAIL"}`},
		} {
			_, err := backend.Execute(context.Background(), mcp.Invocation{Name: tool.name, Arguments: json.RawMessage(tool.args), Scope: scope, Class: mcp.ReadTool})
			if err == nil || !strings.Contains(err.Error(), "scope_rejected") {
				t.Fatalf("mode %d %s: %v", state, tool.name, err)
			}
		}
	}
	for _, state := range []int64{7, 8, 9} {
		mode.Store(state)
		_, err := backend.Execute(context.Background(), mcp.Invocation{Name: mcp.ToolGetCIJobLogs, Arguments: json.RawMessage(`{"job_id":701}`), Scope: scope, Class: mcp.ReadTool})
		if err == nil {
			t.Fatalf("mode %d accepted foreign source/parent", state)
		}
	}
}

func TestActionsReportedHeadIsNotRewrittenToMergeCheckout(t *testing.T) {
	backend, scope, mode := evidenceBackend(t)
	mode.Store(10)
	raw := ciExecute(t, backend, scope, mcp.ToolGetCIJobLogs, `{"job_id":701}`)
	var excerpt githubapi.CILogExcerpt
	if json.Unmarshal(raw, &excerpt) != nil || excerpt.HeadSHA != ciRegressionHead {
		t.Fatalf("reported identity was rewritten: %s", raw)
	}
}
