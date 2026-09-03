package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
			if body.Body != "review body" || body.CommitID != "abc123" || body.Event != wantEvent {
				t.Errorf("review body = %#v, want event %s", body, wantEvent)
			}
			writer.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(writer, `{"id":%d,"state":%q,"body":"review body","commit_id":"abc123"}`, requestNumber, wantEvent)
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
			Body: "review body", CommitID: "abc123", Event: event,
		})
		if err != nil {
			t.Fatalf("SubmitReview(%s) error = %v", event, err)
		}
		if review.State != string(event) || review.CommitID != "abc123" {
			t.Errorf("SubmitReview(%s) = %#v", event, review)
		}
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
