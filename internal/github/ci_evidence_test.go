package github_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
)

func TestRequiredCIInventoryCombinesSourcesAndDoesNotHideUnavailable(t *testing.T) {
	for _, inaccessible := range []bool{false, true} {
		t.Run(fmt.Sprint(inaccessible), func(t *testing.T) {
			client, done := ciClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/acme/widgets/branches/main":
					fmt.Fprint(w, `{"name":"main","protected":true,"commit":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`)
				case "/repos/acme/widgets/branches/main/protection":
					if inaccessible {
						w.WriteHeader(403)
						return
					}
					fmt.Fprint(w, `{"required_status_checks":{"contexts":["build"],"checks":[{"context":"build","app_id":7}]}}`)
				case "/repos/acme/widgets/rules/branches/main":
					fmt.Fprint(w, `[{"type":"required_status_checks","ruleset_id":12,"ruleset_source_type":"Organization","ruleset_source":"acme","parameters":{"required_status_checks":[{"context":"build","integration_id":9}]}},{"type":"required_status_checks","ruleset_id":14,"ruleset_source_type":"Repository","ruleset_source":"acme/widgets","parameters":{"required_status_checks":[{"context":"build","integration_id":11}]}},{"type":"workflows","ruleset_id":13,"ruleset_source_type":"Organization","ruleset_source":"acme","parameters":{"workflows":[{"path":".github/workflows/ci.yml"}]}}]`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer done()
			inventory := client.DiscoverRequiredCI(context.Background(), "token", "acme", "widgets", "main")
			want := 3
			if inaccessible {
				want = 2
			}
			if len(inventory.Requirements) != want || len(inventory.Unresolved) != 1 {
				t.Fatalf("inventory=%+v", inventory)
			}
			apps := map[int64]bool{}
			for _, required := range inventory.Requirements {
				apps[required.AppID] = true
				if required.Context != "build" {
					t.Fatalf("context lost: %+v", required)
				}
			}
			if !apps[9] || !apps[11] || !inaccessible && !apps[7] {
				t.Fatalf("source constraints lost: %+v", inventory.Requirements)
			}
			if inaccessible {
				if inventory.Availability == githubapi.CIAvailable || len(inventory.Failures) == 0 {
					t.Fatalf("inaccessible inventory=%+v", inventory)
				}
			} else if inventory.Availability != githubapi.CIAvailable {
				t.Fatalf("inventory=%+v", inventory)
			}
		})
	}
}

func TestMatchRequiredCIKeepsPendingRerunAndStatusCollision(t *testing.T) {
	now := time.Now().UTC()
	inventory := githubapi.CIInventory{Availability: githubapi.CIAvailable, Requirements: []githubapi.CIRequirement{{Context: "build", Source: "classic", AppID: 7}}}
	observed := githubapi.CIObservation{Availability: githubapi.CIAvailable, Checks: []githubapi.CheckRun{
		{ID: 1, Name: "build", Status: "completed", Conclusion: "success", App: &githubapi.CheckApp{ID: 7}, StartedAt: &now},
		{ID: 2, Name: "build", Status: "queued", App: &githubapi.CheckApp{ID: 7}},
	}, Statuses: []githubapi.CommitStatus{{ID: 3, Context: "build", State: "failure"}}}
	matched := githubapi.MatchRequiredCI(inventory, observed)
	if len(matched) != 2 || matched[0].State != githubapi.CIPending || matched[1].State != githubapi.CIUnresolved {
		t.Fatalf("matched=%+v", matched)
	}
	for _, conclusion := range []string{"failure", "cancelled", "timed_out", "neutral", "skipped", "action_required", "stale", "startup_failure"} {
		observed.Checks = []githubapi.CheckRun{{ID: 2, Name: "build", Status: "completed", Conclusion: conclusion, App: &githubapi.CheckApp{ID: 7}}}
		matched = githubapi.MatchRequiredCI(inventory, observed)
		if matched[0].State != githubapi.CITerminalNonSuccess {
			t.Fatalf("%s: %+v", conclusion, matched)
		}
	}
}
