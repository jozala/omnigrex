package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
)

type httpDoerFunc func(*http.Request) (*http.Response, error)

func (doer httpDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return doer(request)
}

func TestAPIClientResolvesRepositoryInstallationAndCreatesToken(t *testing.T) {
	expiresAt := time.Date(2026, time.September, 2, 13, 0, 0, 0, time.UTC)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("Accept = %q", request.Header.Get("Accept"))
		}
		if request.Header.Get("X-GitHub-Api-Version") != githubapi.APIVersion {
			t.Errorf("X-GitHub-Api-Version = %q", request.Header.Get("X-GitHub-Api-Version"))
		}
		if request.Header.Get("User-Agent") != githubapi.UserAgent {
			t.Errorf("User-Agent = %q", request.Header.Get("User-Agent"))
		}
		if request.Header.Get("Authorization") != "Bearer app-jwt" {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}

		switch requests {
		case 1:
			if request.Method != http.MethodGet || request.URL.EscapedPath() != "/repos/acme/widgets/installation" {
				t.Errorf("request = %s %s", request.Method, request.URL.EscapedPath())
			}
			_, _ = fmt.Fprint(writer, `{"id":73}`)
		case 2:
			if request.Method != http.MethodPost || request.URL.EscapedPath() != "/app/installations/73/access_tokens" {
				t.Errorf("request = %s %s", request.Method, request.URL.EscapedPath())
			}
			if contentType := request.Header.Get("Content-Type"); contentType != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", contentType)
			}
			_, _ = fmt.Fprintf(writer, `{"token":"installation-token","expires_at":%q,"permissions":{"metadata":"read","pull_requests":"write"}}`, expiresAt.Format(time.RFC3339))
		default:
			t.Errorf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatalf("NewAPIClient() error = %v", err)
	}
	installationID, err := client.ResolveRepositoryInstallation(context.Background(), "app-jwt", "acme", "widgets")
	if err != nil {
		t.Fatalf("ResolveRepositoryInstallation() error = %v", err)
	}
	if installationID != 73 {
		t.Errorf("installation ID = %d, want 73", installationID)
	}
	token, err := client.CreateInstallationToken(context.Background(), "app-jwt", installationID)
	if err != nil {
		t.Fatalf("CreateInstallationToken() error = %v", err)
	}
	if token.Token != "installation-token" || !token.ExpiresAt.Equal(expiresAt) ||
		token.Permissions["metadata"] != "read" || token.Permissions["pull_requests"] != "write" {
		t.Errorf("installation token = %#v", token)
	}
}

func TestAPIClientRejectsInvalidBaseURL(t *testing.T) {
	if _, err := githubapi.NewAPIClient(http.DefaultClient, "://invalid"); err == nil {
		t.Fatal("NewAPIClient() error = nil, want invalid base URL error")
	}
	if _, err := githubapi.NewAPIClient(http.DefaultClient, "http://github.example"); err == nil {
		t.Fatal("NewAPIClient() error = nil, want non-loopback HTTP URL rejection")
	}
}

func TestAPIClientClassifiesGitHubFailures(t *testing.T) {
	resetAt := time.Date(2026, time.September, 2, 13, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		status     int
		headers    map[string]string
		assertions func(*testing.T, error)
	}{
		{
			name:   "repository app is not installed",
			status: http.StatusNotFound,
			assertions: func(t *testing.T, err error) {
				var target *githubapi.NotInstalledError
				if !errors.As(err, &target) {
					t.Fatalf("error = %T %v, want NotInstalledError", err, err)
				}
				if target.Owner != "acme" || target.Repository != "widgets" || !target.Permanent() {
					t.Errorf("NotInstalledError = %#v", target)
				}
			},
		},
		{
			name:   "unauthorized",
			status: http.StatusUnauthorized,
			assertions: func(t *testing.T, err error) {
				var target *githubapi.PermissionError
				if !errors.As(err, &target) || !target.Permanent() || target.StatusCode != http.StatusUnauthorized {
					t.Errorf("error = %T %#v, want permanent 401 PermissionError", err, err)
				}
			},
		},
		{
			name:   "forbidden",
			status: http.StatusForbidden,
			assertions: func(t *testing.T, err error) {
				var target *githubapi.PermissionError
				if !errors.As(err, &target) || target.StatusCode != http.StatusForbidden {
					t.Errorf("error = %T %#v, want 403 PermissionError", err, err)
				}
			},
		},
		{
			name:   "forbidden with ordinary rate metadata",
			status: http.StatusForbidden,
			headers: map[string]string{
				"X-RateLimit-Remaining": "42",
				"X-RateLimit-Reset":     fmt.Sprint(resetAt.Unix()),
			},
			assertions: func(t *testing.T, err error) {
				var permission *githubapi.PermissionError
				var rateLimit *githubapi.RateLimitError
				if !errors.As(err, &permission) || errors.As(err, &rateLimit) {
					t.Errorf("error = %T %#v, want PermissionError", err, err)
				}
			},
		},
		{
			name:   "primary rate limit",
			status: http.StatusForbidden,
			headers: map[string]string{
				"Retry-After":           "7",
				"X-RateLimit-Remaining": "0",
				"X-RateLimit-Reset":     fmt.Sprint(resetAt.Unix()),
			},
			assertions: func(t *testing.T, err error) {
				var target *githubapi.RateLimitError
				if !errors.As(err, &target) {
					t.Fatalf("error = %T %v, want RateLimitError", err, err)
				}
				if target.StatusCode != http.StatusForbidden || target.RetryAfter != 7*time.Second || !target.ResetAt.Equal(resetAt) || !target.Transient() {
					t.Errorf("RateLimitError = %#v", target)
				}
			},
		},
		{
			name:   "too many requests",
			status: http.StatusTooManyRequests,
			assertions: func(t *testing.T, err error) {
				var target *githubapi.RateLimitError
				if !errors.As(err, &target) {
					t.Errorf("error = %T %v, want RateLimitError", err, err)
				}
			},
		},
		{
			name:   "request timeout",
			status: http.StatusRequestTimeout,
			assertions: func(t *testing.T, err error) {
				var target *githubapi.TransientError
				if !errors.As(err, &target) || !target.Transient() {
					t.Errorf("error = %T %v, want transient request timeout", err, err)
				}
			},
		},
		{
			name:   "server failure",
			status: http.StatusBadGateway,
			assertions: func(t *testing.T, err error) {
				var target *githubapi.TransientError
				if !errors.As(err, &target) || !target.Transient() {
					t.Errorf("error = %T %v, want TransientError", err, err)
				}
			},
		},
		{
			name:   "other API failure",
			status: http.StatusTeapot,
			assertions: func(t *testing.T, err error) {
				var target *githubapi.APIError
				if !errors.As(err, &target) || target.StatusCode != http.StatusTeapot || target.Message != "failure detail" || target.RequestID != "request-123" {
					t.Errorf("error = %T %#v, want detailed APIError", err, err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				for name, value := range test.headers {
					writer.Header().Set(name, value)
				}
				writer.Header().Set("X-GitHub-Request-Id", "request-123")
				writer.WriteHeader(test.status)
				_, _ = fmt.Fprint(writer, `{"message":"failure detail"}`)
			}))
			defer server.Close()
			client, err := githubapi.NewAPIClient(server.Client(), server.URL)
			if err != nil {
				t.Fatalf("NewAPIClient() error = %v", err)
			}
			_, err = client.ResolveRepositoryInstallation(context.Background(), "app-jwt", "acme", "widgets")
			if err == nil {
				t.Fatal("ResolveRepositoryInstallation() error = nil")
			}
			test.assertions(t, err)
		})
	}
}

func TestAPIClientClassifiesTransportFailuresAsTransient(t *testing.T) {
	transportFailure := errors.New("connection reset")
	client, err := githubapi.NewAPIClient(httpDoerFunc(func(*http.Request) (*http.Response, error) {
		return nil, transportFailure
	}), "https://api.github.test")
	if err != nil {
		t.Fatalf("NewAPIClient() error = %v", err)
	}
	_, err = client.ResolveRepositoryInstallation(context.Background(), "app-jwt", "acme", "widgets")
	var transient *githubapi.TransientError
	if !errors.As(err, &transient) || !errors.Is(err, transportFailure) {
		t.Errorf("error = %T %v, want wrapping TransientError", err, err)
	}
}

func TestAPIClientNeverLeaksRequestCredentialInErrors(t *testing.T) {
	const credential = "phase-seven-secret-token"
	t.Run("transport error", func(t *testing.T) {
		client, err := githubapi.NewAPIClient(httpDoerFunc(func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("proxy rejected %s", credential)
		}), "https://api.github.test")
		if err != nil {
			t.Fatalf("NewAPIClient() error = %v", err)
		}
		_, err = client.GetIssue(context.Background(), credential, "acme", "widgets", 17)
		var transient *githubapi.TransientError
		if !errors.As(err, &transient) {
			t.Errorf("error = %T %v, want TransientError", err, err)
		}
		if err != nil && strings.Contains(err.Error(), credential) {
			t.Errorf("error leaked credential: %v", err)
		}
	})

	t.Run("API error body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprintf(writer, `{"message":"credential %s rejected"}`, credential)
		}))
		defer server.Close()
		client, err := githubapi.NewAPIClient(server.Client(), server.URL)
		if err != nil {
			t.Fatalf("NewAPIClient() error = %v", err)
		}
		_, err = client.GetIssue(context.Background(), credential, "acme", "widgets", 17)
		var permission *githubapi.PermissionError
		if !errors.As(err, &permission) {
			t.Errorf("error = %T %v, want PermissionError", err, err)
		}
		if err != nil && strings.Contains(err.Error(), credential) {
			t.Errorf("error leaked credential: %v", err)
		}
	})
}

func TestAPIClientVerifiesExpectedRepositoryInstallation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(writer, `{"id":73}`)
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatalf("NewAPIClient() error = %v", err)
	}
	if err := client.VerifyRepositoryInstallation(context.Background(), "app-jwt", 73, "acme", "widgets"); err != nil {
		t.Errorf("VerifyRepositoryInstallation() error = %v", err)
	}
	err = client.VerifyRepositoryInstallation(context.Background(), "app-jwt", 74, "acme", "widgets")
	var notInstalled *githubapi.NotInstalledError
	if !errors.As(err, &notInstalled) || notInstalled.InstallationID != 74 || !notInstalled.Permanent() {
		t.Errorf("mismatched installation error = %T %#v, want NotInstalledError for 74", err, err)
	}
}

func TestAPIClientGetsIssueAndValidatesItsIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/repos/acme/widgets/issues/17" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		_, _ = fmt.Fprint(writer, `{"id":1700,"node_id":"I_1700","number":17,"title":"Add durable tools","body":"Details","state":"open","html_url":"https://github.test/acme/widgets/issues/17","user":{"id":7,"login":"octocat"},"labels":[{"name":"enhancement"}]}`)
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatalf("NewAPIClient() error = %v", err)
	}

	issue, err := client.GetIssue(context.Background(), "installation-token", "acme", "widgets", 17)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if issue.ID != 1700 || issue.Number != 17 || issue.Title != "Add durable tools" || issue.User.Login != "octocat" || len(issue.Labels) != 1 {
		t.Errorf("GetIssue() = %#v", issue)
	}

	badServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(writer, `{"id":1800,"number":18,"title":"Wrong issue","state":"open","html_url":"https://github.test/acme/widgets/issues/18"}`)
	}))
	defer badServer.Close()
	badClient, err := githubapi.NewAPIClient(badServer.Client(), badServer.URL)
	if err != nil {
		t.Fatalf("NewAPIClient() error = %v", err)
	}
	if _, err := badClient.GetIssue(context.Background(), "installation-token", "acme", "widgets", 17); !errors.Is(err, githubapi.ErrInvalidAPIResponse) {
		t.Errorf("mismatched GetIssue() error = %T %v, want ErrInvalidAPIResponse", err, err)
	}
}

func TestAPIClientListsIssueCommentsAcrossAllPages(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/repos/acme/widgets/issues/17/comments" || request.URL.Query().Get("per_page") != "100" {
			t.Errorf("request = %s %s?%s", request.Method, request.URL.Path, request.URL.RawQuery)
		}
		if request.URL.Query().Get("page") == "2" {
			_, _ = fmt.Fprint(writer, `[{"id":102,"node_id":"IC_102","body":"second","html_url":"https://github.test/acme/widgets/issues/17#issuecomment-102","issue_url":"https://api.github.test/repos/acme/widgets/issues/17","user":{"id":8,"login":"hubot"},"created_at":"2026-09-03T11:00:00Z","updated_at":"2026-09-03T11:00:00Z"}]`)
			return
		}
		writer.Header().Set("Link", fmt.Sprintf(`<%s/repos/acme/widgets/issues/17/comments?per_page=100&page=2>; rel="next"`, server.URL))
		_, _ = fmt.Fprint(writer, `[{"id":101,"node_id":"IC_101","body":"first","html_url":"https://github.test/acme/widgets/issues/17#issuecomment-101","issue_url":"https://api.github.test/repos/acme/widgets/issues/17","user":{"id":7,"login":"octocat"},"created_at":"2026-09-03T10:00:00Z","updated_at":"2026-09-03T10:00:00Z"}]`)
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatalf("NewAPIClient() error = %v", err)
	}

	comments, err := client.ListIssueComments(context.Background(), "installation-token", "acme", "widgets", 17)
	if err != nil {
		t.Fatalf("ListIssueComments() error = %v", err)
	}
	if len(comments) != 2 || comments[0].ID != 101 || comments[1].ID != 102 || comments[1].User.Login != "hubot" {
		t.Errorf("ListIssueComments() = %#v", comments)
	}
}

func TestAPIClientRejectsCrossOriginPagination(t *testing.T) {
	foreignRequests := 0
	foreign := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		foreignRequests++
	}))
	defer foreign.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Link", fmt.Sprintf(`<%s/stolen>; rel="next"`, foreign.URL))
		_, _ = fmt.Fprint(writer, `[]`)
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatalf("NewAPIClient() error = %v", err)
	}

	_, err = client.ListIssueComments(context.Background(), "installation-token", "acme", "widgets", 17)
	if !errors.Is(err, githubapi.ErrInvalidAPIResponse) {
		t.Errorf("ListIssueComments() error = %T %v, want ErrInvalidAPIResponse", err, err)
	}
	if foreignRequests != 0 {
		t.Errorf("cross-origin server received %d requests", foreignRequests)
	}
}

func TestAPIClientRejectsMalformedIssueURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(writer, `{"id":1700,"node_id":"I_1700","number":17,"title":"Issue","state":"open","html_url":"not a URL"}`)
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatalf("NewAPIClient() error = %v", err)
	}
	if _, err := client.GetIssue(context.Background(), "installation-token", "acme", "widgets", 17); !errors.Is(err, githubapi.ErrInvalidAPIResponse) {
		t.Errorf("GetIssue() error = %T %v, want ErrInvalidAPIResponse", err, err)
	}
}

func TestAPIClientGetsPullRequestAndListsReviewsAcrossAllPages(t *testing.T) {
	const headSHA = "0123456789abcdef0123456789abcdef01234567"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/repos/acme/widgets/pulls/23":
			_, _ = fmt.Fprintf(writer, `{"id":2300,"node_id":"PR_2300","number":23,"title":"Implement tools","body":"Details","state":"open","html_url":"https://github.test/acme/widgets/pull/23","user":{"id":7,"login":"octocat"},"head":{"ref":"feature/tools","sha":%q},"base":{"ref":"main","sha":"1123456789abcdef0123456789abcdef01234567"},"draft":false,"merged":false}`, headSHA)
		case "/repos/acme/widgets/pulls/23/reviews":
			if request.URL.Query().Get("per_page") != "100" {
				t.Errorf("per_page = %q", request.URL.Query().Get("per_page"))
			}
			if request.URL.Query().Get("page") == "2" {
				_, _ = fmt.Fprintf(writer, `[{"id":302,"node_id":"PRR_302","user":{"id":9,"login":"reviewer"},"body":"Please revise","state":"CHANGES_REQUESTED","html_url":"https://github.test/acme/widgets/pull/23#pullrequestreview-302","pull_request_url":"https://api.github.test/repos/acme/widgets/pulls/23","submitted_at":"2026-09-03T11:00:00Z","commit_id":%q}]`, headSHA)
				return
			}
			writer.Header().Set("Link", fmt.Sprintf(`<%s/repos/acme/widgets/pulls/23/reviews?per_page=100&page=2>; rel="next"`, server.URL))
			_, _ = fmt.Fprintf(writer, `[{"id":301,"node_id":"PRR_301","user":{"id":8,"login":"reviewer"},"body":"Looks good","state":"APPROVED","html_url":"https://github.test/acme/widgets/pull/23#pullrequestreview-301","pull_request_url":"https://api.github.test/repos/acme/widgets/pulls/23","submitted_at":"2026-09-03T10:00:00Z","commit_id":%q}]`, headSHA)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatalf("NewAPIClient() error = %v", err)
	}

	pullRequest, err := client.GetPullRequest(context.Background(), "installation-token", "acme", "widgets", 23)
	if err != nil {
		t.Fatalf("GetPullRequest() error = %v", err)
	}
	if pullRequest.ID != 2300 || pullRequest.Number != 23 || pullRequest.Head.SHA != headSHA || pullRequest.Base.Ref != "main" {
		t.Errorf("GetPullRequest() = %#v", pullRequest)
	}
	reviews, err := client.ListPullRequestReviews(context.Background(), "installation-token", "acme", "widgets", 23)
	if err != nil {
		t.Fatalf("ListPullRequestReviews() error = %v", err)
	}
	if len(reviews) != 2 || reviews[0].ID != 301 || reviews[1].State != "CHANGES_REQUESTED" || reviews[1].CommitID != headSHA {
		t.Errorf("ListPullRequestReviews() = %#v", reviews)
	}
}

func TestAPIClientListsAllPullRequestsForExactHeadAcrossAllPages(t *testing.T) {
	const (
		firstHeadSHA  = "0123456789abcdef0123456789abcdef01234567"
		secondHeadSHA = "1123456789abcdef0123456789abcdef01234567"
		baseSHA       = "2123456789abcdef0123456789abcdef01234567"
	)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/repos/acme/widgets/pulls" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		query := request.URL.Query()
		if query.Get("state") != "all" || query.Get("head") != "acme:omnigrex/feature" || query.Get("base") != "main" || query.Get("per_page") != "100" {
			t.Errorf("query = %v", query)
		}
		if query.Get("page") == "2" {
			_, _ = fmt.Fprintf(writer, `[{"id":2400,"node_id":"PR_2400","number":24,"title":"Second attempt","state":"closed","html_url":"https://github.test/acme/widgets/pull/24","head":{"label":"acme:omnigrex/feature","ref":"omnigrex/feature","sha":%q},"base":{"label":"acme:main","ref":"main","sha":%q}}]`, secondHeadSHA, baseSHA)
			return
		}
		writer.Header().Set("Link", fmt.Sprintf(`<%s/repos/acme/widgets/pulls?state=all&head=acme%%3Aomnigrex%%2Ffeature&base=main&per_page=100&page=2>; rel="next"`, server.URL))
		_, _ = fmt.Fprintf(writer, `[{"id":2300,"node_id":"PR_2300","number":23,"title":"First attempt","state":"open","html_url":"https://github.test/acme/widgets/pull/23","head":{"label":"acme:omnigrex/feature","ref":"omnigrex/feature","sha":%q},"base":{"label":"acme:main","ref":"main","sha":%q}}]`, firstHeadSHA, baseSHA)
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}

	pullRequests, err := client.ListPullRequests(context.Background(), "installation-token", "acme", "widgets", githubapi.ListPullRequestsRequest{
		Head: "omnigrex/feature", Base: "main",
	})
	if err != nil {
		t.Fatalf("ListPullRequests() error = %v", err)
	}
	if len(pullRequests) != 2 || pullRequests[0].Number != 23 || pullRequests[1].Number != 24 || pullRequests[1].State != "closed" {
		t.Errorf("ListPullRequests() = %#v", pullRequests)
	}
}

func TestAPIClientListsPullRequestFilesAcrossAllPages(t *testing.T) {
	const (
		modifiedPatch = "@@ -10,2 +10,3 @@ func validate() {\n context\n-old\n+new\n+added"
		removedPatch  = "@@ -4,2 +0,0 @@\n-old\n-gone"
	)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/repos/acme/widgets/pulls/23/files" || request.URL.Query().Get("per_page") != "100" {
			t.Errorf("request = %s %s?%s", request.Method, request.URL.Path, request.URL.RawQuery)
		}
		if request.URL.Query().Get("page") == "2" {
			_, _ = fmt.Fprintf(writer, `[{"filename":"obsolete.txt","status":"removed","patch":%q},{"filename":"assets/logo.png","status":"added"}]`, removedPatch)
			return
		}
		writer.Header().Set("Link", fmt.Sprintf(`<%s/repos/acme/widgets/pulls/23/files?per_page=100&page=2>; rel="next"`, server.URL))
		_, _ = fmt.Fprintf(writer, `[{"filename":"internal/github/api.go","status":"modified","patch":%q}]`, modifiedPatch)
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatalf("NewAPIClient() error = %v", err)
	}

	files, err := client.ListPullRequestFiles(context.Background(), "installation-token", "acme", "widgets", 23)
	if err != nil {
		t.Fatalf("ListPullRequestFiles() error = %v", err)
	}
	if len(files) != 3 || files[0].Filename != "internal/github/api.go" || files[0].Status != githubapi.PullRequestFileModified || files[0].Patch == nil || *files[0].Patch != modifiedPatch {
		t.Fatalf("ListPullRequestFiles() = %#v", files)
	}
	if files[1].Status != githubapi.PullRequestFileRemoved || files[1].Patch == nil || *files[1].Patch != removedPatch {
		t.Errorf("removed file = %#v", files[1])
	}
	if files[2].Filename != "assets/logo.png" || files[2].Status != githubapi.PullRequestFileAdded || files[2].Patch != nil {
		t.Errorf("binary file = %#v", files[2])
	}
}

func TestAPIClientRejectsInvalidPullRequestFileEntries(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "missing filename", body: `[{"status":"modified","patch":"@@ -1 +1 @@\n-old\n+new"}]`},
		{name: "unsafe filename", body: `[{"filename":"internal/../secret","status":"modified","patch":"@@ -1 +1 @@\n-old\n+new"}]`},
		{name: "unknown status", body: `[{"filename":"api.go","status":"moved","patch":"@@ -1 +1 @@\n-old\n+new"}]`},
		{name: "empty patch", body: `[{"filename":"api.go","status":"modified","patch":""}]`},
		{name: "malformed hunk header", body: `[{"filename":"api.go","status":"modified","patch":"@@ invalid @@\n-old\n+new"}]`},
		{name: "malformed hunk suffix", body: `[{"filename":"api.go","status":"modified","patch":"@@ -1 +1 @@invalid\n-old\n+new"}]`},
		{name: "incomplete hunk", body: `[{"filename":"api.go","status":"modified","patch":"@@ -1,2 +1,2 @@\n-old\n+new"}]`},
		{name: "context-only hunk", body: `[{"filename":"api.go","status":"modified","patch":"@@ -1 +1 @@\n unchanged"}]`},
		{name: "empty hunk", body: `[{"filename":"api.go","status":"modified","patch":"@@ -0,0 +0,0 @@\n@@ -1 +1 @@\n-old\n+new"}]`},
		{name: "duplicate filename", body: `[{"filename":"api.go","status":"modified"},{"filename":"api.go","status":"modified"}]`},
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

			if _, err := client.ListPullRequestFiles(context.Background(), "installation-token", "acme", "widgets", 23); !errors.Is(err, githubapi.ErrInvalidAPIResponse) {
				t.Errorf("ListPullRequestFiles() error = %T %v, want ErrInvalidAPIResponse", err, err)
			}
		})
	}
}

func TestAPIClientRejectsCrossOriginPullRequestFilePagination(t *testing.T) {
	foreignRequests := 0
	foreign := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		foreignRequests++
	}))
	defer foreign.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Link", fmt.Sprintf(`<%s/stolen>; rel="next"`, foreign.URL))
		_, _ = fmt.Fprint(writer, `[]`)
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.ListPullRequestFiles(context.Background(), "installation-token", "acme", "widgets", 23)
	if !errors.Is(err, githubapi.ErrInvalidAPIResponse) {
		t.Errorf("ListPullRequestFiles() error = %v, want ErrInvalidAPIResponse", err)
	}
	if foreignRequests != 0 {
		t.Errorf("cross-origin server received %d requests", foreignRequests)
	}
}

func TestAPIClientRejectsDuplicatePullRequestFilePathsAcrossPages(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("page") == "2" {
			_, _ = fmt.Fprint(writer, `[{"filename":"api.go","status":"modified"}]`)
			return
		}
		writer.Header().Set("Link", fmt.Sprintf(`<%s/repos/acme/widgets/pulls/23/files?per_page=100&page=2>; rel="next"`, server.URL))
		_, _ = fmt.Fprint(writer, `[{"filename":"api.go","status":"modified"}]`)
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.ListPullRequestFiles(context.Background(), "installation-token", "acme", "widgets", 23); !errors.Is(err, githubapi.ErrInvalidAPIResponse) {
		t.Errorf("ListPullRequestFiles() error = %v, want ErrInvalidAPIResponse", err)
	}
}

func TestAPIClientRejectsListedPullRequestFromDifferentHeadRepository(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(writer, `[{"id":2300,"node_id":"PR_2300","number":23,"title":"Fork contribution","state":"open","html_url":"https://github.test/acme/widgets/pull/23","head":{"label":"fork:omnigrex/feature","ref":"omnigrex/feature","sha":"0123456789abcdef0123456789abcdef01234567"},"base":{"label":"acme:main","ref":"main","sha":"1123456789abcdef0123456789abcdef01234567"}}]`)
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.ListPullRequests(context.Background(), "installation-token", "acme", "widgets", githubapi.ListPullRequestsRequest{Head: "omnigrex/feature"})
	if !errors.Is(err, githubapi.ErrInvalidAPIResponse) {
		t.Errorf("ListPullRequests() error = %v, want ErrInvalidAPIResponse", err)
	}
}

func TestAPIClientRejectsCrossOriginPullRequestPagination(t *testing.T) {
	foreignRequests := 0
	foreign := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		foreignRequests++
	}))
	defer foreign.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Link", fmt.Sprintf(`<%s/stolen>; rel="next"`, foreign.URL))
		_, _ = fmt.Fprint(writer, `[]`)
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.ListPullRequests(context.Background(), "installation-token", "acme", "widgets", githubapi.ListPullRequestsRequest{Head: "omnigrex/feature"})
	if !errors.Is(err, githubapi.ErrInvalidAPIResponse) {
		t.Errorf("ListPullRequests() error = %v, want ErrInvalidAPIResponse", err)
	}
	if foreignRequests != 0 {
		t.Errorf("cross-origin server received %d requests", foreignRequests)
	}
}

func TestAPIClientListsReviewThreadsAcrossAllPages(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		assertReviewThreadsGraphQLRequest(t, request)
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		decodeRequestJSON(t, request, &payload)
		switch requests {
		case 1:
			if !strings.Contains(payload.Query, "reviewThreads(first: 100, after: $threadsCursor)") || payload.Variables["owner"] != "acme" || payload.Variables["name"] != "widgets" || payload.Variables["number"] != float64(23) || payload.Variables["threadsCursor"] != nil {
				t.Errorf("initial GraphQL payload = %#v", payload)
			}
			root := reviewCommentGraphQLFixture("PRRC_401", 401, "reviewer", "Validate this", false, false)
			writeGraphQLData(t, writer, reviewThreadsGraphQLFixture(2, graphQLPageFixture(true, false, "thread-start", "thread-next"), []any{
				reviewThreadGraphQLFixture("PRRT_401", false, false, 2, graphQLPageFixture(true, false, "comment-start", "comment-next"), []any{root}),
			}))
		case 2:
			if !strings.Contains(payload.Query, "... on PullRequestReviewThread") || payload.Variables["threadID"] != "PRRT_401" || payload.Variables["commentsCursor"] != "comment-next" {
				t.Errorf("comment continuation payload = %#v", payload)
			}
			reply := reviewCommentGraphQLFixture("PRRC_402", 402, "developer", "Fixed", false, true)
			writeGraphQLData(t, writer, map[string]any{"node": reviewThreadGraphQLFixture("PRRT_401", false, false, 2, graphQLPageFixture(false, true, "comment-last", "comment-last"), []any{reply})})
		case 3:
			if !strings.Contains(payload.Query, "repository(owner: $owner, name: $name)") || payload.Variables["threadsCursor"] != "thread-next" {
				t.Errorf("thread continuation payload = %#v", payload)
			}
			outdated := reviewCommentGraphQLFixture("PRRC_403", 403, "reviewer", "Old location", true, false)
			writeGraphQLData(t, writer, reviewThreadsGraphQLFixture(2, graphQLPageFixture(false, true, "thread-last", "thread-last"), []any{
				reviewThreadGraphQLFixture("PRRT_403", true, true, 1, graphQLPageFixture(false, false, "outdated-comment", "outdated-comment"), []any{outdated}),
			}))
		default:
			t.Errorf("unexpected request %d", requests)
		}
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatalf("NewAPIClient() error = %v", err)
	}

	threads, err := client.ListReviewThreads(context.Background(), "installation-token", "acme", "widgets", 23)
	if err != nil {
		t.Fatalf("ListReviewThreads() error = %v", err)
	}
	if requests != 3 || len(threads) != 2 || threads[0].ID != "PRRT_401" || threads[0].Resolved || threads[0].Outdated || len(threads[0].Comments) != 2 || threads[0].Comments[1].ID != 402 || threads[0].Comments[1].InReplyToID != 401 || threads[0].Comments[0].Side != "RIGHT" {
		t.Errorf("ListReviewThreads() = %#v", threads)
	}
	if reviewID := threads[0].Comments[0].PullRequestReviewID; reviewID == nil || *reviewID != 302 {
		t.Errorf("PullRequestReviewID = %v, want 302", reviewID)
	}
	if threads[1].ID != "PRRT_403" || !threads[1].Resolved || !threads[1].Outdated || threads[1].Comments[0].Line != nil || threads[1].Comments[0].OriginalLine == nil || *threads[1].Comments[0].OriginalLine != 42 || threads[1].Comments[0].User.Login != "reviewer" {
		t.Errorf("outdated resolved thread = %#v", threads[1])
	}
}

func TestAPIClientAcceptsStandaloneReviewThreadRootAndReply(t *testing.T) {
	root := standaloneReviewCommentGraphQLFixture("PRRC_401", 401, "reviewer", "Validate this", false, false)
	reply := standaloneReviewCommentGraphQLFixture("PRRC_402", 402, "developer", "Fixed", false, true)
	data := reviewThreadsGraphQLFixture(1, graphQLPageFixture(false, false, "thread", "thread"), []any{
		reviewThreadGraphQLFixture("PRRT_401", false, false, 2, graphQLPageFixture(false, false, "comment-start", "comment-end"), []any{root, reply}),
	})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertReviewThreadsGraphQLRequest(t, request)
		writeGraphQLData(t, writer, data)
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}

	threads, err := client.ListReviewThreads(context.Background(), "installation-token", "acme", "widgets", 23)
	if err != nil {
		t.Fatalf("ListReviewThreads() error = %v", err)
	}
	if len(threads) != 1 || len(threads[0].Comments) != 2 {
		t.Fatalf("ListReviewThreads() = %#v", threads)
	}
	if threads[0].Comments[0].PullRequestReviewID != nil || threads[0].Comments[1].PullRequestReviewID != nil || threads[0].Comments[1].InReplyToID != 401 {
		t.Errorf("standalone review comments = %#v", threads[0].Comments)
	}
}

func TestAPIClientAcceptsCapturedGitHubSingleLineReviewThreadShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertReviewThreadsGraphQLRequest(t, request)
		writeGraphQLData(t, writer, capturedGitHubSingleLineReviewThreadFixture())
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}

	threads, err := client.ListReviewThreads(context.Background(), "installation-token", "acme", "widgets", 23)
	if err != nil {
		t.Fatalf("ListReviewThreads() error = %v", err)
	}
	if len(threads) != 1 || len(threads[0].Comments) != 1 {
		t.Fatalf("ListReviewThreads() = %#v", threads)
	}
	comment := threads[0].Comments[0]
	if comment.Line == nil || *comment.Line != 909 || comment.StartLine != nil || comment.OriginalLine == nil || *comment.OriginalLine != 884 || comment.OriginalStartLine != nil || comment.Side != "RIGHT" || comment.StartSide != "" {
		t.Errorf("single-line comment = %#v", comment)
	}
}

func TestAPIClientRejectsMalformedNeighborsOfCapturedGitHubSingleLineReviewThreadShape(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any, map[string]any)
	}{
		{name: "duplicate endpoint with start side", mutate: func(thread, _ map[string]any) { thread["startDiffSide"] = "RIGHT" }},
		{name: "non-endpoint start without original range", mutate: func(thread, _ map[string]any) { thread["startLine"] = 908 }},
		{name: "comment also duplicates endpoint", mutate: func(_, comment map[string]any) { comment["startLine"] = 909 }},
		{name: "collapsed current multiline range", mutate: func(thread, comment map[string]any) {
			thread["startDiffSide"] = "RIGHT"
			thread["originalStartLine"] = 883
			comment["originalStartLine"] = 883
		}},
		{name: "outdated thread retains current location", mutate: func(thread, _ map[string]any) { thread["isOutdated"] = true }},
		{name: "file thread retains line location", mutate: func(thread, comment map[string]any) {
			thread["subjectType"] = "FILE"
			comment["subjectType"] = "FILE"
		}},
		{name: "invalid diff side", mutate: func(thread, _ map[string]any) { thread["diffSide"] = "BOTH" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := capturedGitHubSingleLineReviewThreadFixture()
			thread := firstReviewThreadFixture(data)
			comment := firstReviewCommentFixture(data)
			test.mutate(thread, comment)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writeGraphQLData(t, writer, data)
			}))
			defer server.Close()
			client, err := githubapi.NewAPIClient(server.Client(), server.URL)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.ListReviewThreads(context.Background(), "installation-token", "acme", "widgets", 23); !errors.Is(err, githubapi.ErrInvalidAPIResponse) {
				t.Errorf("ListReviewThreads() error = %T %v, want ErrInvalidAPIResponse", err, err)
			}
		})
	}
}

func TestAPIClientRejectsCapturedSingleLineThreadShapeChangeDuringCommentPagination(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		data := capturedGitHubSingleLineReviewThreadFixture()
		thread := firstReviewThreadFixture(data)
		comments := thread["comments"].(map[string]any)
		comments["totalCount"] = 2
		if requests == 1 {
			comments["pageInfo"] = graphQLPageFixture(true, false, "captured-comment", "captured-comment-next")
			writeGraphQLData(t, writer, data)
			return
		}

		thread["startLine"] = nil
		comment := firstReviewCommentFixture(data)
		comment["id"] = "PRRC_kwDOExample5"
		comment["fullDatabaseId"] = "2048099918"
		comment["body"] = "Updated."
		comment["url"] = "https://github.test/acme/widgets/pull/23#discussion_r2048099918"
		comment["replyTo"] = map[string]any{"id": "PRRC_kwDOExample4", "fullDatabaseId": "2048099917"}
		comments["pageInfo"] = graphQLPageFixture(false, true, "captured-comment-last", "captured-comment-last")
		writeGraphQLData(t, writer, map[string]any{"node": thread})
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.ListReviewThreads(context.Background(), "installation-token", "acme", "widgets", 23); !errors.Is(err, githubapi.ErrInvalidAPIResponse) {
		t.Errorf("ListReviewThreads() error = %T %v, want ErrInvalidAPIResponse", err, err)
	}
	if requests != 2 {
		t.Errorf("requests = %d, want 2", requests)
	}
}

func TestAPIClientListsEmptyReviewThreads(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertReviewThreadsGraphQLRequest(t, request)
		writeGraphQLData(t, writer, reviewThreadsGraphQLFixture(0, graphQLPageFixture(false, false, "", ""), []any{}))
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	threads, err := client.ListReviewThreads(context.Background(), "installation-token", "acme", "widgets", 23)
	if err != nil {
		t.Fatalf("ListReviewThreads() error = %v", err)
	}
	if threads == nil || len(threads) != 0 {
		t.Errorf("ListReviewThreads() = %#v, want non-nil empty slice", threads)
	}
}

func TestAPIClientRejectsInvalidGraphQLReviewThreadResponses(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "missing repository", mutate: func(data map[string]any) { data["repository"] = nil }},
		{name: "mismatched repository owner", mutate: func(data map[string]any) {
			data["repository"].(map[string]any)["owner"] = map[string]any{"login": "other"}
		}},
		{name: "mismatched Pull Request", mutate: func(data map[string]any) {
			data["repository"].(map[string]any)["pullRequest"].(map[string]any)["number"] = 24
		}},
		{name: "missing pageInfo", mutate: func(data map[string]any) {
			data["repository"].(map[string]any)["pullRequest"].(map[string]any)["reviewThreads"].(map[string]any)["pageInfo"] = nil
		}},
		{name: "next page without cursor", mutate: func(data map[string]any) {
			pageInfo := data["repository"].(map[string]any)["pullRequest"].(map[string]any)["reviewThreads"].(map[string]any)["pageInfo"].(map[string]any)
			pageInfo["hasNextPage"] = true
			pageInfo["endCursor"] = nil
		}},
		{name: "duplicate comment node id", mutate: func(data map[string]any) {
			thread := firstReviewThreadFixture(data)
			comments := thread["comments"].(map[string]any)
			comment := comments["nodes"].([]any)[0]
			comments["totalCount"] = 2
			comments["nodes"] = []any{comment, comment}
		}},
		{name: "duplicate thread node id", mutate: func(data map[string]any) {
			connection := data["repository"].(map[string]any)["pullRequest"].(map[string]any)["reviewThreads"].(map[string]any)
			duplicate := reviewThreadGraphQLFixture("PRRT_401", true, false, 1, graphQLPageFixture(false, false, "other-comment", "other-comment"), []any{reviewCommentGraphQLFixture("PRRC_499", 499, "reviewer", "Other", false, false)})
			connection["totalCount"] = 2
			connection["nodes"] = []any{connection["nodes"].([]any)[0], duplicate}
		}},
		{name: "missing author", mutate: func(data map[string]any) { firstReviewCommentFixture(data)["author"] = nil }},
		{name: "present review missing database id", mutate: func(data map[string]any) {
			firstReviewCommentFixture(data)["pullRequestReview"] = map[string]any{"fullDatabaseId": nil}
		}},
		{name: "present review with zero database id", mutate: func(data map[string]any) {
			firstReviewCommentFixture(data)["pullRequestReview"] = map[string]any{"fullDatabaseId": "0"}
		}},
		{name: "present review with negative database id", mutate: func(data map[string]any) {
			firstReviewCommentFixture(data)["pullRequestReview"] = map[string]any{"fullDatabaseId": "-1"}
		}},
		{name: "present review with malformed database id", mutate: func(data map[string]any) {
			firstReviewCommentFixture(data)["pullRequestReview"] = map[string]any{"fullDatabaseId": "review-302"}
		}},
		{name: "unsafe path", mutate: func(data map[string]any) { firstReviewThreadFixture(data)["path"] = "../api.go" }},
		{name: "mismatched comment path", mutate: func(data map[string]any) { firstReviewCommentFixture(data)["path"] = "other.go" }},
		{name: "mismatched comment URL", mutate: func(data map[string]any) {
			firstReviewCommentFixture(data)["url"] = "https://github.test/acme/other/pull/23#discussion_r401"
		}},
		{name: "updated before created", mutate: func(data map[string]any) { firstReviewCommentFixture(data)["updatedAt"] = "2026-09-03T09:00:00Z" }},
		{name: "missing current location", mutate: func(data map[string]any) { firstReviewCommentFixture(data)["line"] = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			comment := reviewCommentGraphQLFixture("PRRC_401", 401, "reviewer", "Finding", false, false)
			data := reviewThreadsGraphQLFixture(1, graphQLPageFixture(false, false, "thread", "thread"), []any{
				reviewThreadGraphQLFixture("PRRT_401", false, false, 1, graphQLPageFixture(false, false, "comment", "comment"), []any{comment}),
			})
			test.mutate(data)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writeGraphQLData(t, writer, data)
			}))
			defer server.Close()
			client, err := githubapi.NewAPIClient(server.Client(), server.URL)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.ListReviewThreads(context.Background(), "token", "acme", "widgets", 23); !errors.Is(err, githubapi.ErrInvalidAPIResponse) {
				t.Errorf("ListReviewThreads() error = %T %v, want ErrInvalidAPIResponse", err, err)
			}
		})
	}
}

func TestAPIClientRejectsMismatchedReviewCommentContinuation(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		if requests == 1 {
			comment := reviewCommentGraphQLFixture("PRRC_401", 401, "reviewer", "Finding", false, false)
			writeGraphQLData(t, writer, reviewThreadsGraphQLFixture(1, graphQLPageFixture(false, false, "thread", "thread"), []any{
				reviewThreadGraphQLFixture("PRRT_401", false, false, 2, graphQLPageFixture(true, false, "comment", "comment-next"), []any{comment}),
			}))
			return
		}
		reply := reviewCommentGraphQLFixture("PRRC_402", 402, "developer", "Fixed", false, true)
		thread := reviewThreadGraphQLFixture("PRRT_401", false, false, 2, graphQLPageFixture(false, true, "last", "last"), []any{reply})
		thread["pullRequest"].(map[string]any)["number"] = 24
		writeGraphQLData(t, writer, map[string]any{"node": thread})
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListReviewThreads(context.Background(), "token", "acme", "widgets", 23); !errors.Is(err, githubapi.ErrInvalidAPIResponse) {
		t.Errorf("ListReviewThreads() error = %T %v, want ErrInvalidAPIResponse", err, err)
	}
}

func TestAPIClientRejectsGraphQLReviewThreadErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(writer, `{"data":{"repository":null},"errors":[{"message":"resource not accessible"}]}`)
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListReviewThreads(context.Background(), "token", "acme", "widgets", 23); !errors.Is(err, githubapi.ErrInvalidAPIResponse) {
		t.Errorf("ListReviewThreads() error = %T %v, want ErrInvalidAPIResponse", err, err)
	}
}

func TestAPIClientClassifiesReviewThreadTransportErrors(t *testing.T) {
	transportFailure := errors.New("connection reset")
	client, err := githubapi.NewAPIClient(httpDoerFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.Path != "/graphql" || request.Header.Get("Authorization") != "Bearer installation-token" {
			t.Errorf("request = %s %s, authorization %q", request.Method, request.URL.Path, request.Header.Get("Authorization"))
		}
		return nil, transportFailure
	}), "https://api.github.test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ListReviewThreads(context.Background(), "installation-token", "acme", "widgets", 23)
	var transient *githubapi.TransientError
	if !errors.As(err, &transient) || !errors.Is(err, transportFailure) {
		t.Errorf("error = %T %v, want wrapping TransientError", err, err)
	}
}

func TestAPIClientGetsCheckRunsForExactCommitAcrossAllPages(t *testing.T) {
	const headSHA = "0123456789abcdef0123456789abcdef01234567"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/repos/acme/widgets/commits/"+headSHA+"/check-runs" || request.URL.Query().Get("per_page") != "100" {
			t.Errorf("request = %s %s?%s", request.Method, request.URL.Path, request.URL.RawQuery)
		}
		if request.URL.Query().Get("page") == "2" {
			_, _ = fmt.Fprintf(writer, `{"total_count":2,"check_runs":[{"id":502,"node_id":"CR_502","name":"lint","head_sha":%q,"status":"completed","conclusion":"success","html_url":"https://github.test/acme/widgets/runs/502","details_url":"https://ci.test/502","completed_at":"2026-09-03T11:00:00Z"}]}`, headSHA)
			return
		}
		writer.Header().Set("Link", fmt.Sprintf(`<%s/repos/acme/widgets/commits/%s/check-runs?per_page=100&page=2>; rel="next"`, server.URL, headSHA))
		_, _ = fmt.Fprintf(writer, `{"total_count":2,"check_runs":[{"id":501,"node_id":"CR_501","name":"test","head_sha":%q,"status":"in_progress","conclusion":null,"html_url":"https://github.test/acme/widgets/runs/501","details_url":"https://ci.test/501"}]}`, headSHA)
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatalf("NewAPIClient() error = %v", err)
	}

	checks, err := client.GetCheckRuns(context.Background(), "installation-token", "acme", "widgets", headSHA)
	if err != nil {
		t.Fatalf("GetCheckRuns() error = %v", err)
	}
	if len(checks) != 2 || checks[0].Name != "test" || checks[1].Conclusion != "success" || checks[1].HeadSHA != headSHA {
		t.Errorf("GetCheckRuns() = %#v", checks)
	}
}

func TestAPIClientRejectsIncompletePaginatedResponses(t *testing.T) {
	const headSHA = "0123456789abcdef0123456789abcdef01234567"
	tests := []struct {
		name string
		body string
		call func(*githubapi.APIClient) error
	}{
		{
			name: "Issue comment without updated timestamp",
			body: `[{"id":101,"node_id":"IC_101","body":"comment","html_url":"https://github.test/acme/widgets/issues/17#issuecomment-101","issue_url":"https://api.github.test/repos/acme/widgets/issues/17","created_at":"2026-09-03T10:00:00Z"}]`,
			call: func(client *githubapi.APIClient) error {
				_, err := client.ListIssueComments(context.Background(), "token", "acme", "widgets", 17)
				return err
			},
		},
		{
			name: "review with unknown state",
			body: fmt.Sprintf(`[{"id":301,"node_id":"PRR_301","state":"UNKNOWN","html_url":"https://github.test/acme/widgets/pull/23#review","pull_request_url":"https://api.github.test/repos/acme/widgets/pulls/23","submitted_at":"2026-09-03T10:00:00Z","commit_id":%q}]`, headSHA),
			call: func(client *githubapi.APIClient) error {
				_, err := client.ListPullRequestReviews(context.Background(), "token", "acme", "widgets", 23)
				return err
			},
		},
		{
			name: "review thread with zero current line",
			body: fmt.Sprintf(`[{"id":401,"node_id":"PRRC_401","body":"finding","path":"api.go","commit_id":%q,"original_commit_id":%q,"html_url":"https://github.test/acme/widgets/pull/23#discussion_r401","pull_request_url":"https://api.github.test/repos/acme/widgets/pulls/23","created_at":"2026-09-03T10:00:00Z","updated_at":"2026-09-03T10:00:00Z","line":0,"side":"RIGHT","position":2,"subject_type":"line"}]`, headSHA, headSHA),
			call: func(client *githubapi.APIClient) error {
				_, err := client.ListReviewThreads(context.Background(), "token", "acme", "widgets", 23)
				return err
			},
		},
		{
			name: "completed check without conclusion",
			body: fmt.Sprintf(`{"total_count":1,"check_runs":[{"id":501,"node_id":"CR_501","name":"test","head_sha":%q,"status":"completed","html_url":"https://github.test/acme/widgets/runs/501","completed_at":"2026-09-03T10:00:00Z"}]}`, headSHA),
			call: func(client *githubapi.APIClient) error {
				_, err := client.GetCheckRuns(context.Background(), "token", "acme", "widgets", headSHA)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(writer, test.body)
			}))
			defer server.Close()
			client, err := githubapi.NewAPIClient(server.Client(), server.URL)
			if err != nil {
				t.Fatalf("NewAPIClient() error = %v", err)
			}
			if err := test.call(client); !errors.Is(err, githubapi.ErrInvalidAPIResponse) {
				t.Errorf("error = %T %v, want ErrInvalidAPIResponse", err, err)
			}
		})
	}
}

func TestAPIClientListsRepositoryAndIssueLabelsAcrossAllPages(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer installation-token" {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		if request.URL.Query().Get("per_page") != "100" {
			t.Errorf("per_page = %q, want 100", request.URL.Query().Get("per_page"))
		}
		page := request.URL.Query().Get("page")
		switch request.URL.Path {
		case "/repos/acme/widgets/labels":
			if page == "" {
				writer.Header().Set("Link", fmt.Sprintf(`<%s/repos/acme/widgets/labels?per_page=100&page=2>; rel="next"`, server.URL))
				_, _ = fmt.Fprint(writer, `[{"name":"repository-one","color":"111111","description":"first"}]`)
				return
			}
			_, _ = fmt.Fprint(writer, `[{"name":"repository-two","color":"222222","description":"second"}]`)
		case "/repos/acme/widgets/issues/17/labels":
			if page == "" {
				writer.Header().Set("Link", fmt.Sprintf(`<%s/repos/acme/widgets/issues/17/labels?per_page=100&page=2>; rel="next"`, server.URL))
				_, _ = fmt.Fprint(writer, `[{"name":"issue-one"}]`)
				return
			}
			_, _ = fmt.Fprint(writer, `[{"name":"issue-two"}]`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatalf("NewAPIClient() error = %v", err)
	}

	repositoryLabels, err := client.ListRepositoryLabels(context.Background(), "installation-token", "acme", "widgets")
	if err != nil {
		t.Fatalf("ListRepositoryLabels() error = %v", err)
	}
	if len(repositoryLabels) != 2 || repositoryLabels[0].Name != "repository-one" || repositoryLabels[1].Name != "repository-two" {
		t.Errorf("repository labels = %#v", repositoryLabels)
	}
	issueLabels, err := client.ListIssueLabels(context.Background(), "installation-token", "acme", "widgets", 17)
	if err != nil {
		t.Fatalf("ListIssueLabels() error = %v", err)
	}
	if len(issueLabels) != 2 || issueLabels[0].Name != "issue-one" || issueLabels[1].Name != "issue-two" {
		t.Errorf("issue labels = %#v", issueLabels)
	}
}

func TestAPIClientCreatesAndReplacesLabelsAndSubmitsNativeReviews(t *testing.T) {
	const headSHA = "0123456789abcdef0123456789abcdef01234567"
	requestNumber := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestNumber++
		switch requestNumber {
		case 1:
			if request.Method != http.MethodPost || request.URL.Path != "/repos/acme/widgets/labels" {
				t.Errorf("create label request = %s %s", request.Method, request.URL.Path)
			}
			var body githubapi.Label
			decodeRequestJSON(t, request, &body)
			if body.Name != "omnigrex:run" || body.Color != "1f6feb" || body.Description != "Start work" {
				t.Errorf("create label body = %#v", body)
			}
			writer.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(writer, `{"name":"omnigrex:run","color":"1f6feb","description":"Start work"}`)
		case 2:
			if request.Method != http.MethodPut || request.URL.Path != "/repos/acme/widgets/issues/17/labels" {
				t.Errorf("replace labels request = %s %s", request.Method, request.URL.Path)
			}
			var body struct {
				Labels []string `json:"labels"`
			}
			decodeRequestJSON(t, request, &body)
			if fmt.Sprint(body.Labels) != "[external omnigrex:reviewing]" {
				t.Errorf("replacement labels = %v", body.Labels)
			}
			_, _ = fmt.Fprint(writer, `[{"name":"external"},{"name":"omnigrex:reviewing"}]`)
		case 3, 4:
			if request.Method != http.MethodPost || request.URL.Path != "/repos/acme/widgets/pulls/23/reviews" {
				t.Errorf("review request = %s %s", request.Method, request.URL.Path)
			}
			var body struct {
				Body     string                `json:"body"`
				CommitID string                `json:"commit_id"`
				Event    githubapi.ReviewEvent `json:"event"`
			}
			decodeRequestJSON(t, request, &body)
			wantEvent := githubapi.ReviewApprove
			if requestNumber == 4 {
				wantEvent = githubapi.ReviewRequestChanges
			}
			if body.Body != "review body" || body.CommitID != headSHA || body.Event != wantEvent {
				t.Errorf("review body = %#v, want event %s", body, wantEvent)
			}
			wantState := "APPROVED"
			if wantEvent == githubapi.ReviewRequestChanges {
				wantState = "CHANGES_REQUESTED"
			}
			writer.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(writer, `{"id":%d,"node_id":"PRR_%d","state":%q,"body":"review body","commit_id":%q,"html_url":"https://github.test/acme/widgets/pull/23#review","pull_request_url":"https://api.github.test/repos/acme/widgets/pulls/23","submitted_at":"2026-09-03T12:00:00Z"}`, requestNumber, requestNumber, wantState, headSHA)
		default:
			t.Errorf("unexpected request %d", requestNumber)
		}
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatalf("NewAPIClient() error = %v", err)
	}

	created, err := client.CreateRepositoryLabel(context.Background(), "installation-token", "acme", "widgets", githubapi.Label{
		Name: "omnigrex:run", Color: "1f6feb", Description: "Start work",
	})
	if err != nil || created.Name != "omnigrex:run" {
		t.Fatalf("CreateRepositoryLabel() = %#v, %v", created, err)
	}
	replaced, err := client.ReplaceIssueLabels(context.Background(), "installation-token", "acme", "widgets", 17, []string{"external", "omnigrex:reviewing"})
	if err != nil || len(replaced) != 2 {
		t.Fatalf("ReplaceIssueLabels() = %#v, %v", replaced, err)
	}
	for _, event := range []githubapi.ReviewEvent{githubapi.ReviewApprove, githubapi.ReviewRequestChanges} {
		review, err := client.SubmitReview(context.Background(), "installation-token", "acme", "widgets", 23, githubapi.ReviewRequest{
			Body: "review body", CommitID: headSHA, Event: event,
		})
		if err != nil {
			t.Fatalf("SubmitReview(%s) error = %v", event, err)
		}
		if review.CommitID != headSHA || review.State != map[githubapi.ReviewEvent]string{githubapi.ReviewApprove: "APPROVED", githubapi.ReviewRequestChanges: "CHANGES_REQUESTED"}[event] {
			t.Errorf("SubmitReview(%s) = %#v", event, review)
		}
	}
}

func TestAPIClientOpensLinkedPullRequestAndCreatesMarkedComments(t *testing.T) {
	const (
		headSHA = "0123456789abcdef0123456789abcdef01234567"
		baseSHA = "1123456789abcdef0123456789abcdef01234567"
		marker  = "<!-- omnigrex:operation op-123 -->"
	)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		switch requests {
		case 1:
			if request.Method != http.MethodPost || request.URL.Path != "/repos/acme/widgets/pulls" {
				t.Errorf("open Pull Request request = %s %s", request.Method, request.URL.Path)
			}
			var body struct {
				Title string `json:"title"`
				Body  string `json:"body"`
				Head  string `json:"head"`
				Base  string `json:"base"`
			}
			decodeRequestJSON(t, request, &body)
			wantBody := "Implements API primitives.\n\nCloses #17\n\n" + marker
			if body.Title != "Implement GitHub primitives" || body.Body != wantBody || body.Head != "feature/github-api" || body.Base != "main" {
				t.Errorf("open Pull Request body = %#v", body)
			}
			writer.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(writer, `{"id":2300,"node_id":"PR_2300","number":23,"title":"Implement GitHub primitives","body":%q,"state":"open","html_url":"https://github.test/acme/widgets/pull/23","head":{"ref":"feature/github-api","sha":%q},"base":{"ref":"main","sha":%q}}`, wantBody, headSHA, baseSHA)
		case 2, 3:
			wantNumber := 17
			wantText := "Issue update"
			if requests == 3 {
				wantNumber = 23
				wantText = "Pull Request update"
			}
			if request.Method != http.MethodPost || request.URL.Path != fmt.Sprintf("/repos/acme/widgets/issues/%d/comments", wantNumber) {
				t.Errorf("comment request = %s %s", request.Method, request.URL.Path)
			}
			var body struct {
				Body string `json:"body"`
			}
			decodeRequestJSON(t, request, &body)
			wantBody := wantText + "\n\n" + marker
			if body.Body != wantBody {
				t.Errorf("comment body = %q, want %q", body.Body, wantBody)
			}
			writer.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(writer, `{"id":%d,"node_id":"IC_%d","body":%q,"html_url":"https://github.test/acme/widgets/issues/%d#issuecomment-%d","issue_url":"https://api.github.test/repos/acme/widgets/issues/%d","created_at":"2026-09-03T10:00:00Z","updated_at":"2026-09-03T10:00:00Z"}`, 600+requests, 600+requests, wantBody, wantNumber, 600+requests, wantNumber)
		default:
			t.Errorf("unexpected request %d", requests)
		}
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatalf("NewAPIClient() error = %v", err)
	}

	pullRequest, err := client.OpenPullRequest(context.Background(), "installation-token", "acme", "widgets", githubapi.OpenPullRequestRequest{
		Title: "Implement GitHub primitives", Body: "Implements API primitives.", Head: "feature/github-api", Base: "main", IssueNumber: 17, Marker: marker,
	})
	if err != nil || pullRequest.Number != 23 || pullRequest.Head.SHA != headSHA {
		t.Fatalf("OpenPullRequest() = %#v, %v", pullRequest, err)
	}
	issueComment, err := client.CreateIssueComment(context.Background(), "installation-token", "acme", "widgets", 17, githubapi.CommentRequest{Body: "Issue update", Marker: marker})
	if err != nil || issueComment.ID != 602 {
		t.Fatalf("CreateIssueComment() = %#v, %v", issueComment, err)
	}
	pullRequestComment, err := client.CreatePullRequestComment(context.Background(), "installation-token", "acme", "widgets", 23, githubapi.CommentRequest{Body: "Pull Request update", Marker: marker})
	if err != nil || pullRequestComment.ID != 603 {
		t.Fatalf("CreatePullRequestComment() = %#v, %v", pullRequestComment, err)
	}
}

func TestAPIClientSubmitsCommitBoundReviewWithValidatedInlineComments(t *testing.T) {
	const headSHA = "0123456789abcdef0123456789abcdef01234567"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/repos/acme/widgets/pulls/23/reviews" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		var body githubapi.ReviewRequest
		decodeRequestJSON(t, request, &body)
		if body.CommitID != headSHA || body.Event != githubapi.ReviewRequestChanges || len(body.Comments) != 2 {
			t.Errorf("review body = %#v", body)
		}
		if body.Comments[0].Path != "internal/github/api.go" || body.Comments[0].Line != 42 || body.Comments[0].Side != githubapi.ReviewSideRight {
			t.Errorf("single-line comment = %#v", body.Comments[0])
		}
		if body.Comments[1].StartLine != 50 || body.Comments[1].Line != 53 || body.Comments[1].StartSide != githubapi.ReviewSideRight {
			t.Errorf("multi-line comment = %#v", body.Comments[1])
		}
		writer.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(writer, `{"id":701,"node_id":"PRR_701","state":"CHANGES_REQUESTED","body":"Please address these findings.","commit_id":%q,"html_url":"https://github.test/acme/widgets/pull/23#pullrequestreview-701","pull_request_url":"https://api.github.test/repos/acme/widgets/pulls/23","submitted_at":"2026-09-03T12:00:00Z"}`, headSHA)
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatalf("NewAPIClient() error = %v", err)
	}

	review, err := client.SubmitReview(context.Background(), "installation-token", "acme", "widgets", 23, githubapi.ReviewRequest{
		Body: "Please address these findings.", CommitID: headSHA, Event: githubapi.ReviewRequestChanges,
		Comments: []githubapi.ReviewCommentRequest{
			{Path: "internal/github/api.go", Body: "Check this value.", Line: 42, Side: githubapi.ReviewSideRight},
			{Path: "internal/github/api_test.go", Body: "This range is incomplete.", StartLine: 50, StartSide: githubapi.ReviewSideRight, Line: 53, Side: githubapi.ReviewSideRight},
		},
	})
	if err != nil {
		t.Fatalf("SubmitReview() error = %v", err)
	}
	if review.ID != 701 || review.NodeID != "PRR_701" || review.CommitID != headSHA || review.State != "CHANGES_REQUESTED" {
		t.Errorf("SubmitReview() = %#v", review)
	}
}

func TestAPIClientDeletesIssueCommentBySharedCommentID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodDelete || request.URL.Path != "/repos/acme/widgets/issues/comments/602" {
			t.Errorf("delete comment request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer installation-token" {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatalf("NewAPIClient() error = %v", err)
	}

	if err := client.DeleteIssueComment(context.Background(), "installation-token", "acme", "widgets", 602); err != nil {
		t.Fatalf("DeleteIssueComment() error = %v", err)
	}
}

func TestAPIClientAddsAndRemovesSpecificIssueLabels(t *testing.T) {
	requestNumber := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestNumber++
		switch requestNumber {
		case 1:
			if request.Method != http.MethodPost || request.URL.Path != "/repos/acme/widgets/issues/17/labels" {
				t.Errorf("add labels request = %s %s", request.Method, request.URL.Path)
			}
			var body struct {
				Labels []string `json:"labels"`
			}
			decodeRequestJSON(t, request, &body)
			if fmt.Sprint(body.Labels) != "[omnigrex:reviewing]" {
				t.Errorf("added labels = %v", body.Labels)
			}
			_, _ = fmt.Fprint(writer, `[{"name":"omnigrex:reviewing"}]`)
		case 2:
			if request.Method != http.MethodDelete || request.URL.EscapedPath() != "/repos/acme/widgets/issues/17/labels/omnigrex:developing" {
				t.Errorf("remove label request = %s %s", request.Method, request.URL.EscapedPath())
			}
			_, _ = fmt.Fprint(writer, `[]`)
		default:
			t.Errorf("unexpected request %d", requestNumber)
		}
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatalf("NewAPIClient() error = %v", err)
	}
	if _, err := client.AddIssueLabels(context.Background(), "installation-token", "acme", "widgets", 17, []string{"omnigrex:reviewing"}); err != nil {
		t.Fatalf("AddIssueLabels() error = %v", err)
	}
	if err := client.RemoveIssueLabel(context.Background(), "installation-token", "acme", "widgets", 17, "omnigrex:developing"); err != nil {
		t.Fatalf("RemoveIssueLabel() error = %v", err)
	}
}

func TestAPIClientRejectsInvalidRequestsAsPermanentConfigurationErrors(t *testing.T) {
	requests := 0
	client, err := githubapi.NewAPIClient(httpDoerFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("unexpected HTTP request")
	}), "https://api.github.test")
	if err != nil {
		t.Fatalf("NewAPIClient() error = %v", err)
	}
	tests := []struct {
		name  string
		cause error
		call  func() error
	}{
		{
			name:  "empty repository owner",
			cause: githubapi.ErrInvalidRepository,
			call: func() error {
				_, err := client.ResolveRepositoryInstallation(context.Background(), "app-jwt", "", "widgets")
				return err
			},
		},
		{
			name:  "empty credential",
			cause: githubapi.ErrMissingCredential,
			call: func() error {
				_, err := client.ListRepositoryLabels(context.Background(), "", "acme", "widgets")
				return err
			},
		},
		{
			name:  "invalid label color",
			cause: githubapi.ErrInvalidLabel,
			call: func() error {
				_, err := client.CreateRepositoryLabel(context.Background(), "token", "acme", "widgets", githubapi.Label{Name: "name", Color: "#ffffff"})
				return err
			},
		},
		{
			name:  "empty replacement label",
			cause: githubapi.ErrInvalidLabel,
			call: func() error {
				_, err := client.ReplaceIssueLabels(context.Background(), "token", "acme", "widgets", 1, []string{""})
				return err
			},
		},
		{
			name:  "request changes without body",
			cause: githubapi.ErrInvalidReview,
			call: func() error {
				_, err := client.SubmitReview(context.Background(), "token", "acme", "widgets", 1, githubapi.ReviewRequest{Event: githubapi.ReviewRequestChanges})
				return err
			},
		},
		{
			name:  "invalid Issue number",
			cause: githubapi.ErrInvalidIssueNumber,
			call: func() error {
				_, err := client.GetIssue(context.Background(), "token", "acme", "widgets", 0)
				return err
			},
		},
		{
			name:  "invalid Pull Request number",
			cause: githubapi.ErrInvalidPullRequestNumber,
			call: func() error {
				_, err := client.ListReviewThreads(context.Background(), "token", "acme", "widgets", 0)
				return err
			},
		},
		{
			name:  "invalid Pull Request file-list number",
			cause: githubapi.ErrInvalidPullRequestNumber,
			call: func() error {
				_, err := client.ListPullRequestFiles(context.Background(), "token", "acme", "widgets", 0)
				return err
			},
		},
		{
			name:  "identical Pull Request head and base filters",
			cause: githubapi.ErrInvalidPullRequest,
			call: func() error {
				_, err := client.ListPullRequests(context.Background(), "token", "acme", "widgets", githubapi.ListPullRequestsRequest{Head: "main", Base: "main"})
				return err
			},
		},
		{
			name:  "invalid check-run commit",
			cause: githubapi.ErrInvalidCommitSHA,
			call: func() error {
				_, err := client.GetCheckRuns(context.Background(), "token", "acme", "widgets", "main")
				return err
			},
		},
		{
			name:  "Pull Request without linked Issue",
			cause: githubapi.ErrInvalidIssueNumber,
			call: func() error {
				_, err := client.OpenPullRequest(context.Background(), "token", "acme", "widgets", githubapi.OpenPullRequestRequest{Title: "title", Head: "feature", Base: "main"})
				return err
			},
		},
		{
			name:  "malformed hidden marker",
			cause: githubapi.ErrInvalidMarker,
			call: func() error {
				_, err := client.CreateIssueComment(context.Background(), "token", "acme", "widgets", 1, githubapi.CommentRequest{Body: "body", Marker: "not hidden"})
				return err
			},
		},
		{
			name:  "empty comment",
			cause: githubapi.ErrInvalidComment,
			call: func() error {
				_, err := client.CreatePullRequestComment(context.Background(), "token", "acme", "widgets", 1, githubapi.CommentRequest{})
				return err
			},
		},
		{
			name:  "review without commit",
			cause: githubapi.ErrInvalidCommitSHA,
			call: func() error {
				_, err := client.SubmitReview(context.Background(), "token", "acme", "widgets", 1, githubapi.ReviewRequest{Body: "approved", Event: githubapi.ReviewApprove})
				return err
			},
		},
		{
			name:  "inline review comment without side",
			cause: githubapi.ErrInvalidReview,
			call: func() error {
				_, err := client.SubmitReview(context.Background(), "token", "acme", "widgets", 1, githubapi.ReviewRequest{Body: "review", CommitID: "0123456789abcdef0123456789abcdef01234567", Event: githubapi.ReviewRequestChanges, Comments: []githubapi.ReviewCommentRequest{{Path: "api.go", Body: "finding", Line: 1}}})
				return err
			},
		},
		{
			name:  "inline review range ending before start",
			cause: githubapi.ErrInvalidReview,
			call: func() error {
				_, err := client.SubmitReview(context.Background(), "token", "acme", "widgets", 1, githubapi.ReviewRequest{Body: "review", CommitID: "0123456789abcdef0123456789abcdef01234567", Event: githubapi.ReviewRequestChanges, Comments: []githubapi.ReviewCommentRequest{{Path: "api.go", Body: "finding", StartLine: 4, StartSide: githubapi.ReviewSideRight, Line: 3, Side: githubapi.ReviewSideRight}}})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.call()
			var configuration *githubapi.ConfigurationError
			if !errors.Is(err, test.cause) || !errors.As(err, &configuration) || !configuration.Permanent() {
				t.Errorf("error = %T %v, want permanent ConfigurationError wrapping %v", err, err, test.cause)
			}
		})
	}
	if requests != 0 {
		t.Errorf("invalid requests made %d HTTP calls, want 0", requests)
	}
}

func decodeRequestJSON(t *testing.T, request *http.Request, destination any) {
	t.Helper()
	defer request.Body.Close()
	if err := json.NewDecoder(request.Body).Decode(destination); err != nil {
		t.Fatalf("decode request JSON: %v", err)
	}
}

func assertReviewThreadsGraphQLRequest(t *testing.T, request *http.Request) {
	t.Helper()
	if request.Method != http.MethodPost || request.URL.EscapedPath() != "/graphql" || request.URL.RawQuery != "" {
		t.Errorf("request = %s %s?%s, want POST /graphql", request.Method, request.URL.EscapedPath(), request.URL.RawQuery)
	}
	if request.Header.Get("Authorization") != "Bearer installation-token" {
		t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
	}
	if request.Header.Get("Accept") != "application/vnd.github+json" || request.Header.Get("Content-Type") != "application/json" {
		t.Errorf("content headers = Accept %q Content-Type %q", request.Header.Get("Accept"), request.Header.Get("Content-Type"))
	}
	if request.Header.Get("X-GitHub-Api-Version") != githubapi.APIVersion || request.Header.Get("User-Agent") != githubapi.UserAgent {
		t.Errorf("GitHub headers = version %q user-agent %q", request.Header.Get("X-GitHub-Api-Version"), request.Header.Get("User-Agent"))
	}
}

func graphQLPageFixture(hasNextPage, hasPreviousPage bool, startCursor, endCursor string) map[string]any {
	page := map[string]any{"hasNextPage": hasNextPage, "hasPreviousPage": hasPreviousPage, "startCursor": nil, "endCursor": nil}
	if startCursor != "" {
		page["startCursor"] = startCursor
	}
	if endCursor != "" {
		page["endCursor"] = endCursor
	}
	return page
}

func reviewCommentGraphQLFixture(nodeID string, databaseID int64, login, body string, outdated, reply bool) map[string]any {
	const commitSHA = "0123456789abcdef0123456789abcdef01234567"
	line := any(42)
	if outdated {
		line = nil
	}
	comment := map[string]any{
		"id": nodeID, "fullDatabaseId": fmt.Sprint(databaseID), "body": body, "path": "internal/github/api.go",
		"commit": map[string]any{"oid": commitSHA}, "originalCommit": map[string]any{"oid": commitSHA},
		"url": "https://github.test/acme/widgets/pull/23#discussion_r" + fmt.Sprint(databaseID), "author": map[string]any{"login": login},
		"createdAt": "2026-09-03T10:00:00Z", "updatedAt": "2026-09-03T11:00:00Z", "line": line, "startLine": nil,
		"originalLine": 42, "originalStartLine": nil, "subjectType": "LINE", "pullRequestReview": map[string]any{"fullDatabaseId": "302"}, "replyTo": nil,
	}
	if reply {
		comment["replyTo"] = map[string]any{"id": "PRRC_401", "fullDatabaseId": "401"}
	}
	return comment
}

func standaloneReviewCommentGraphQLFixture(nodeID string, databaseID int64, login, body string, outdated, reply bool) map[string]any {
	comment := reviewCommentGraphQLFixture(nodeID, databaseID, login, body, outdated, reply)
	comment["pullRequestReview"] = nil
	return comment
}

func reviewThreadGraphQLFixture(nodeID string, resolved, outdated bool, totalComments int, pageInfo map[string]any, comments []any) map[string]any {
	line := any(42)
	if outdated {
		line = nil
	}
	return map[string]any{
		"id": nodeID, "isResolved": resolved, "isOutdated": outdated, "path": "internal/github/api.go",
		"diffSide": "RIGHT", "startDiffSide": nil, "line": line, "startLine": nil, "originalLine": 42, "originalStartLine": nil, "subjectType": "LINE",
		"pullRequest": graphQLPullRequestIdentityFixture(),
		"comments":    map[string]any{"totalCount": totalComments, "pageInfo": pageInfo, "nodes": comments},
	}
}

func capturedGitHubSingleLineReviewThreadFixture() map[string]any {
	const commitSHA = "0123456789abcdef0123456789abcdef01234567"
	comment := map[string]any{
		"id": "PRRC_kwDOExample4", "fullDatabaseId": "2048099917", "body": "Keep the validation at the API boundary.", "path": "internal/github/api.go",
		"commit": map[string]any{"oid": commitSHA}, "originalCommit": map[string]any{"oid": commitSHA},
		"url": "https://github.test/acme/widgets/pull/23#discussion_r2048099917", "author": map[string]any{"login": "reviewer"},
		"createdAt": "2026-09-03T10:00:00Z", "updatedAt": "2026-09-03T10:04:31Z", "line": 909, "startLine": nil,
		"originalLine": 884, "originalStartLine": nil, "position": 37, "subjectType": "LINE",
		"pullRequestReview": map[string]any{"fullDatabaseId": "302"}, "replyTo": nil,
	}
	thread := map[string]any{
		"id": "PRRT_kwDOExample9", "isResolved": false, "isOutdated": false, "path": "internal/github/api.go",
		"diffSide": "RIGHT", "startDiffSide": nil, "line": 909, "startLine": 909, "originalLine": 884, "originalStartLine": nil, "subjectType": "LINE",
		"pullRequest": graphQLPullRequestIdentityFixture(),
		"comments":    map[string]any{"totalCount": 1, "pageInfo": graphQLPageFixture(false, false, "captured-comment", "captured-comment"), "nodes": []any{comment}},
	}
	return reviewThreadsGraphQLFixture(1, graphQLPageFixture(false, false, "captured-thread", "captured-thread"), []any{thread})
}

func reviewThreadsGraphQLFixture(totalThreads int, pageInfo map[string]any, threads []any) map[string]any {
	repository := graphQLRepositoryIdentityFixture()
	pullRequest := graphQLPullRequestIdentityFixture()
	pullRequest["reviewThreads"] = map[string]any{"totalCount": totalThreads, "pageInfo": pageInfo, "nodes": threads}
	repository["pullRequest"] = pullRequest
	return map[string]any{"repository": repository}
}

func graphQLPullRequestIdentityFixture() map[string]any {
	return map[string]any{"id": "PR_23", "number": 23, "repository": graphQLRepositoryIdentityFixture()}
}

func graphQLRepositoryIdentityFixture() map[string]any {
	return map[string]any{"id": "R_widgets", "name": "widgets", "owner": map[string]any{"login": "acme"}}
}

func firstReviewThreadFixture(data map[string]any) map[string]any {
	return data["repository"].(map[string]any)["pullRequest"].(map[string]any)["reviewThreads"].(map[string]any)["nodes"].([]any)[0].(map[string]any)
}

func firstReviewCommentFixture(data map[string]any) map[string]any {
	return firstReviewThreadFixture(data)["comments"].(map[string]any)["nodes"].([]any)[0].(map[string]any)
}

func writeGraphQLData(t *testing.T, writer http.ResponseWriter, data map[string]any) {
	t.Helper()
	if err := json.NewEncoder(writer).Encode(map[string]any{"data": data}); err != nil {
		t.Errorf("encode GraphQL fixture: %v", err)
	}
}
