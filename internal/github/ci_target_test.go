package github_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	githubapi "github.com/jozala/omnigrex/internal/github"
)

func TestCITargetIndependentBranchTip(t *testing.T) {
	for _, scenario := range []string{"stable", "branch moved", "base moved", "head moved", "head ref moved", "base ref moved", "repo moved", "repo name moved", "PR moved", "merge moved", "closed", "merged", "unmergeable", "unknown mergeability", "reversed parents", "wrong base", "wrong head", "one parent", "three parents", "wrong commit", "invalid branch", "wrong branch", "PR unavailable", "branch unavailable", "commit unavailable", "fresh PR unavailable", "fresh branch unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			head, base, tip, merge := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("d", 40)
			prReads, branchReads := 0, 0
			client, done := ciClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var response any
				switch r.URL.Path {
				case "/repos/acme/widgets/pulls/17":
					prReads++
					if scenario == "PR unavailable" || scenario == "fresh PR unavailable" && prReads == 2 {
						http.Error(w, "unavailable", 403)
						return
					}
					pr := githubapi.PullRequest{ID: 17, NodeID: "PR_17", Number: 17, Title: "fixture", State: "open", HTMLURL: "https://github.test/acme/widgets/pull/17"}
					// Decode the fixture to avoid coupling to nested API field type names.
					raw := `{"head":{"ref":"feature","sha":"` + head + `"},"base":{"ref":"main","sha":"` + base + `","repo":{"id":9123,"full_name":"acme/widgets"}},"mergeable":true,"merge_commit_sha":"` + merge + `"}`
					if err := json.Unmarshal([]byte(raw), &pr); err != nil {
						t.Fatal(err)
					}
					if prReads == 2 {
						switch scenario {
						case "base moved":
							pr.Base.SHA = tip
						case "head moved":
							pr.Head.SHA = tip
						case "head ref moved":
							pr.Head.Ref = "other"
						case "base ref moved":
							pr.Base.Ref = "other"
						case "repo moved":
							pr.Base.Repository.ID++
						case "repo name moved":
							pr.Base.Repository.FullName = "acme/other"
						case "PR moved":
							pr.ID++
						case "merge moved":
							pr.MergeCommitSHA = &base
						case "closed":
							pr.State = "closed"
						case "merged":
							pr.Merged = true
						case "unmergeable":
							v := false
							pr.Mergeable = &v
						case "unknown mergeability":
							pr.Mergeable = nil
						}
					}
					response = pr
				case "/repos/acme/widgets/branches/main":
					branchReads++
					if scenario == "branch unavailable" || scenario == "fresh branch unavailable" && branchReads == 2 {
						http.Error(w, "unavailable", 403)
						return
					}
					sha, name := tip, "main"
					if scenario == "branch moved" && branchReads == 2 {
						sha = base
					}
					if scenario == "invalid branch" {
						sha = "invalid"
					}
					if scenario == "wrong branch" {
						name = "other"
					}
					response = map[string]any{"name": name, "commit": map[string]string{"sha": sha}}
				case "/repos/acme/widgets/git/commits/" + merge:
					if scenario == "commit unavailable" {
						http.Error(w, "unavailable", 403)
						return
					}
					parents, sha := []string{tip, head}, merge
					switch scenario {
					case "reversed parents":
						parents = []string{head, tip}
					case "wrong base":
						parents[0] = base
					case "wrong head":
						parents[1] = base
					case "one parent":
						parents = parents[:1]
					case "three parents":
						parents = append(parents, base)
					case "wrong commit":
						sha = base
					}
					values := []map[string]string{}
					for _, p := range parents {
						values = append(values, map[string]string{"sha": p})
					}
					response = map[string]any{"sha": sha, "parents": values}
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				if err := json.NewEncoder(w).Encode(response); err != nil {
					t.Error(err)
				}
			}))
			defer done()
			mapping := client.VerifyCITarget(context.Background(), "token", "acme", "widgets", 9123, 17, 17, head, "feature", "main")
			if scenario == "stable" {
				if mapping.State != "verified" || mapping.BaseSHA != tip || mapping.TestMergeSHA != merge || prReads != 2 || branchReads != 2 {
					t.Fatalf("mapping=%+v reads=%d/%d", mapping, prReads, branchReads)
				}
			} else if mapping.State == "verified" || mapping.TestMergeSHA != "" {
				t.Fatalf("unsafe mapping: %+v", mapping)
			}
		})
	}
}
