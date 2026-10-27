package github_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
)

func TestRequiredCIEmptyVersusIncompletePolicy(t *testing.T) {
	for _, tc := range []struct {
		name      string
		protected bool
		classic   string
		status    int
		rules     string
		available githubapi.CIAvailability
	}{
		{"unprotected", false, ``, 200, `[]`, githubapi.CIAvailable},
		{"protected without CI", true, `{"required_status_checks":null}`, 200, `[]`, githubapi.CIAvailable},
		{"missing protection field", true, `{}`, 200, `[]`, githubapi.CIUnavailable},
		{"permission denied", false, ``, 403, ``, githubapi.CIUnavailable},
		{"rate limited", false, ``, 429, ``, githubapi.CIUnavailable},
		{"transient", false, ``, 503, ``, githubapi.CIUnavailable},
		{"malformed rules", false, ``, 200, `null`, githubapi.CIUnavailable},
		{"required unknown rule", false, ``, 200, `[{"type":"future_ci_rule","ruleset_id":1,"ruleset_source_type":"Organization","ruleset_source":"acme"}]`, githubapi.CIAvailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, done := ciClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/acme/widgets/branches/main":
					fmt.Fprintf(w, `{"name":"main","protected":%t,"commit":{"sha":%q}}`, tc.protected, ciHeadSHA)
				case "/repos/acme/widgets/branches/main/protection":
					fmt.Fprint(w, tc.classic)
				case "/repos/acme/widgets/rules/branches/main":
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.rules)
				default:
					t.Errorf("unexpected policy endpoint: %s", r.URL.Path)
					w.WriteHeader(500)
				}
			}))
			defer done()
			inventory := client.DiscoverRequiredCI(context.Background(), "token", "acme", "widgets", "main")
			if inventory.Availability != tc.available || len(inventory.Requirements) != 0 {
				t.Fatalf("inventory=%+v", inventory)
			}
			if tc.available != githubapi.CIAvailable && len(inventory.Failures) == 0 {
				t.Fatalf("missing failure: %+v", inventory)
			}
			if tc.name == "required unknown rule" && len(inventory.Unresolved) != 1 {
				t.Fatalf("unknown rule disappeared: %+v", inventory)
			}
		})
	}
}

func TestCIStatusPaginationAndCurrentResult(t *testing.T) {
	for _, wrongPage := range []bool{false, true} {
		t.Run(fmt.Sprint(wrongPage), func(t *testing.T) {
			client, done := ciClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("page") == "" {
					path := "/repos/acme/widgets/commits/" + ciHeadSHA + "/statuses?per_page=100&page=2"
					if wrongPage {
						path = "/repos/acme/foreign/commits/" + ciHeadSHA + "/statuses?per_page=100&page=2"
					}
					w.Header().Set("Link", `<http://`+r.Host+path+`>; rel="next"`)
					fmt.Fprintf(w, `[{"id":1,"node_id":"CS_1","context":"legacy","state":"success","url":"https://api.github.test/repos/acme/widgets/statuses/%s","created_at":"2026-10-06T10:00:00Z","updated_at":"2026-10-06T10:00:00Z"}]`, ciHeadSHA)
				} else {
					fmt.Fprintf(w, `[{"id":2,"node_id":"CS_2","context":"legacy","state":"pending","url":"https://api.github.test/repos/acme/widgets/statuses/%s","created_at":"2026-10-06T11:00:00Z","updated_at":"2026-10-06T11:00:00Z"}]`, ciHeadSHA)
				}
			}))
			defer done()
			statuses, err := client.GetCommitStatuses(context.Background(), "token", "acme", "widgets", ciHeadSHA)
			if wrongPage {
				if err == nil {
					t.Fatal("foreign pagination accepted")
				}
				return
			}
			if err != nil || len(statuses) != 2 {
				t.Fatalf("statuses=%+v, %v", statuses, err)
			}
			inventory := githubapi.CIInventory{Availability: githubapi.CIAvailable, Requirements: []githubapi.CIRequirement{{Context: "legacy", Source: "classic"}}}
			results := githubapi.MatchRequiredCI(inventory, githubapi.CIObservation{Availability: githubapi.CIAvailable, Statuses: statuses})
			if len(results) != 1 || results[0].State != githubapi.CIPending || results[0].ResultID != 2 {
				t.Fatalf("results=%+v", results)
			}
		})
	}
}

func TestCIEvidenceBoundedContinuationPreservesAllCollections(t *testing.T) {
	inventory := githubapi.CIInventory{Availability: githubapi.CIAvailable, ObservedAt: time.Now(), Requirements: []githubapi.CIRequirement{}}
	observed := githubapi.CIObservation{Availability: githubapi.CIAvailable, RepositoryID: 9123, HeadSHA: ciHeadSHA, ObservedAt: time.Now()}
	for i := 0; i < 47; i++ {
		name := fmt.Sprintf("build-%02d", i)
		inventory.Requirements = append(inventory.Requirements, githubapi.CIRequirement{Context: name, Source: "classic"})
		observed.Checks = append(observed.Checks, githubapi.CheckRun{ID: int64(i + 1), Name: name, HeadSHA: ciHeadSHA, Status: "queued", App: &githubapi.CheckApp{ID: 7}})
	}
	mapping := githubapi.CITargetMapping{RepositoryID: 9123, ReviewedHead: ciHeadSHA, State: "head_only", ObservedAt: time.Now()}
	cursor := ""
	seen := 0
	for pageNumber := 0; pageNumber < 5; pageNumber++ {
		page, err := githubapi.PageCIEvidence(inventory, []githubapi.CIObservation{observed}, mapping, cursor)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(page)
		if len(raw) > githubapi.MaxCISerializedResponseBytes {
			t.Fatal("oversized evidence")
		}
		seen += len(page.CheckRuns)
		if !page.HasMore {
			break
		}
		if cursor == page.NextCursor {
			t.Fatal("cursor did not advance")
		}
		cursor = page.NextCursor
		inventory.ObservedAt = time.Now()
		observed.ObservedAt = time.Now()
		mapping.ObservedAt = time.Now()
	}
	if seen != 47 {
		t.Fatalf("saw %d checks", seen)
	}
	bad := mapping
	bad.RepositoryID++
	if _, err := githubapi.PageCIEvidence(inventory, []githubapi.CIObservation{observed}, bad, cursor); err == nil {
		t.Fatal("foreign cursor accepted")
	}
	observed.Checks[46].Status = "in_progress"
	if _, err := githubapi.PageCIEvidence(inventory, []githubapi.CIObservation{observed}, mapping, cursor); err == nil {
		t.Fatal("changed-result cursor accepted")
	}
	if _, err := githubapi.PageCIEvidence(inventory, []githubapi.CIObservation{observed}, mapping, strings.Repeat("x", 3000)); err == nil {
		t.Fatal("malformed cursor accepted")
	}
}

func TestRulesetOnlyProtectionDistinguishesExplicitAbsenceFromInaccessibleClassic(t *testing.T) {
	for _, message := range []string{"Branch not protected", "Not Found"} {
		t.Run(message, func(t *testing.T) {
			client, done := ciClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/acme/widgets/branches/main":
					fmt.Fprintf(w, `{"name":"main","protected":true,"commit":{"sha":%q}}`, ciHeadSHA)
				case "/repos/acme/widgets/branches/main/protection":
					w.WriteHeader(404)
					fmt.Fprintf(w, `{"message":%q}`, message)
				case "/repos/acme/widgets/rules/branches/main":
					fmt.Fprint(w, `[{"type":"required_status_checks","ruleset_id":12,"ruleset_source_type":"Organization","ruleset_source":"acme","parameters":{"required_status_checks":[{"context":"build","integration_id":7}]}}]`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer done()
			inventory := client.DiscoverRequiredCI(context.Background(), "token", "acme", "widgets", "main")
			want := githubapi.CIAvailable
			if message != "Branch not protected" {
				want = githubapi.CIPartial
			}
			if inventory.Availability != want || len(inventory.Requirements) != 1 || inventory.Requirements[0].RulesetID != 12 {
				t.Fatalf("inventory=%+v", inventory)
			}
		})
	}
}
