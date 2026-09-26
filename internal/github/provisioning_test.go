package github_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	githubapi "github.com/jozala/omnigrex/internal/github"
)

func TestAPIClientListsInstallationRepositoriesAcrossPages(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/installation/repositories" {
			t.Errorf("path = %s, want /installation/repositories", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer installation-token" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		query := request.URL.Query()
		if query.Get("per_page") != "100" {
			t.Errorf("per_page = %q", query.Get("per_page"))
		}
		if query.Get("page") == "2" {
			_, _ = fmt.Fprint(writer, `{"total_count":3,"repositories":[{"id":3,"name":"three","full_name":"acme/three"}]}`)
			return
		}
		if query.Get("page") != "1" {
			t.Errorf("page = %q, want 1 or 2", query.Get("page"))
		}
		_, _ = fmt.Fprint(writer, `{"total_count":3,"repositories":[
			{"id":1,"name":"one","full_name":"acme/one"},
			{"id":2,"name":"two","full_name":"acme/two"}]}`)
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatalf("NewAPIClient() error = %v", err)
	}

	repositories, err := client.ListInstallationRepositories(context.Background(), "installation-token")
	if err != nil {
		t.Fatalf("ListInstallationRepositories() error = %v", err)
	}
	if len(repositories) != 3 || repositories[0].ID != 1 || repositories[0].Owner != "acme" || repositories[2].Name != "three" {
		t.Errorf("repositories = %#v, want three acme repositories", repositories)
	}
}

func TestAPIClientRejectsInvalidInstallationRepositories(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "missing total", body: `{"repositories":[]}`},
		{name: "incomplete entry", body: `{"total_count":1,"repositories":[{"id":0,"name":"","full_name":""}]}`},
		{name: "full name mismatch", body: `{"total_count":1,"repositories":[{"id":1,"name":"one","full_name":"acme/two"}]}`},
		{name: "count mismatch", body: `{"total_count":2,"repositories":[{"id":1,"name":"one","full_name":"acme/one"}]}`},
		{name: "duplicate id", body: `{"total_count":2,"repositories":[{"id":1,"name":"one","full_name":"acme/one"},{"id":1,"name":"two","full_name":"acme/two"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(writer, test.body)
			}))
			defer server.Close()
			client, err := githubapi.NewAPIClient(server.Client(), server.URL)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.ListInstallationRepositories(context.Background(), "installation-token"); !errors.Is(err, githubapi.ErrInvalidAPIResponse) {
				t.Errorf("error = %v, want ErrInvalidAPIResponse", err)
			}
		})
	}
}

func TestAPIClientGetsRepositoryAndVerifiesIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.EscapedPath() != "/repos/acme/widgets" {
			t.Errorf("path = %s", request.URL.EscapedPath())
		}
		_, _ = fmt.Fprint(writer, `{"id":9123,"name":"widgets","full_name":"acme/widgets","owner":{"login":"acme"}}`)
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatalf("NewAPIClient() error = %v", err)
	}

	repository, err := client.GetRepository(context.Background(), "installation-token", "acme", "widgets")
	if err != nil {
		t.Fatalf("GetRepository() error = %v", err)
	}
	if repository.ID != 9123 || repository.Owner != "acme" || repository.Name != "widgets" {
		t.Errorf("repository = %#v", repository)
	}

	mismatch := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(writer, `{"id":9999,"name":"widgets","full_name":"acme/widgets","owner":{"login":"acme"}}`)
	}))
	defer mismatch.Close()
	mismatchClient, err := githubapi.NewAPIClient(mismatch.Client(), mismatch.URL)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := mismatchClient.GetRepository(context.Background(), "installation-token", "acme", "widgets")
	if err != nil {
		t.Fatalf("GetRepository() error = %v", err)
	}
	if observed.ID == 9123 {
		t.Errorf("mismatched repository ID was not observable: %#v", observed)
	}
}

func TestInstallationEnumeratorMintsTokenThenLists(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.Method+" "+request.URL.Path)
		if request.URL.Path == "/app/installations/99/access_tokens" {
			_, _ = fmt.Fprint(writer, `{"token":"installation-token","expires_at":"2026-09-03T11:00:00Z","permissions":{}}`)
			return
		}
		_, _ = fmt.Fprint(writer, `{"total_count":1,"repositories":[{"id":9123,"name":"omnigrex","full_name":"jozala/omnigrex"}]}`)
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	enumerator, err := githubapi.NewInstallationEnumerator(&fixedProvisioningJWT{token: "app-jwt"}, client)
	if err != nil {
		t.Fatal(err)
	}
	repositories, err := enumerator.EnumerateInstallationRepositories(context.Background(), 99)
	if err != nil {
		t.Fatalf("Enumerate() error = %v", err)
	}
	if len(repositories) != 1 || repositories[0].ID != 9123 {
		t.Errorf("repositories = %#v", repositories)
	}
	if len(paths) != 2 || paths[0] != "POST /app/installations/99/access_tokens" {
		t.Errorf("requests = %#v, want token mint then list", paths)
	}
}

type fixedProvisioningJWT struct{ token string }

func (provider *fixedProvisioningJWT) AppJWT(context.Context) (string, error) {
	return provider.token, nil
}
