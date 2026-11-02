package github_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	githubapi "github.com/jozala/omnigrex/internal/github"
)

func TestDecisionAuthorityRequiresExactActorAndWritePermission(t *testing.T) {
	for _, permission := range []string{"read", "triage", "write", "maintain", "admin"} {
		t.Run(permission, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/owner/repo/collaborators/maintainer/permission" || r.Method != "GET" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				fmt.Fprintf(w, `{"permission":%q,"user":{"id":42,"login":"maintainer"}}`, permission)
			}))
			defer server.Close()
			client, err := githubapi.NewAPIClient(server.Client(), server.URL)
			if err != nil {
				t.Fatal(err)
			}
			authorized, err := client.CanDecideWorkItem(context.Background(), "fixture-token", "owner", "repo", "maintainer", 42)
			want := permission == "write" || permission == "maintain" || permission == "admin"
			if err != nil || authorized != want {
				t.Fatalf("authority=%t %v", authorized, err)
			}
			if ok, err := client.CanDecideWorkItem(context.Background(), "fixture-token", "owner", "repo", "maintainer", 99); err == nil || ok {
				t.Fatal("permission accepted for wrong actor")
			}
		})
	}
}
