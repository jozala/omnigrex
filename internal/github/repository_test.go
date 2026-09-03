package github_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	githubapi "github.com/jozala/omnigrex/internal/github"
)

func TestAPIClientResolvesDefaultBranchCommitAndFetchesExactFile(t *testing.T) {
	const commitSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	wantContent := []byte("---\nruntime: opencode-acp/v1\n---\nInstructions.\n")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.Header.Get("Authorization") != "Bearer installation-token" {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		switch requests {
		case 1:
			if request.Method != http.MethodGet || request.URL.EscapedPath() != "/repos/acme/widgets" || request.URL.RawQuery != "" {
				t.Errorf("repository request = %s %s?%s", request.Method, request.URL.EscapedPath(), request.URL.RawQuery)
			}
			_, _ = fmt.Fprint(writer, `{"default_branch":"trunk/production"}`)
		case 2:
			if request.Method != http.MethodGet || request.URL.EscapedPath() != "/repos/acme/widgets/commits/trunk%2Fproduction" {
				t.Errorf("commit request = %s %s", request.Method, request.URL.EscapedPath())
			}
			_, _ = fmt.Fprintf(writer, `{"sha":%q}`, commitSHA)
		case 3:
			if request.Method != http.MethodGet || request.URL.EscapedPath() != "/repos/acme/widgets/contents/.omnigrex/team/developer.md" {
				t.Errorf("contents request = %s %s", request.Method, request.URL.EscapedPath())
			}
			if request.URL.Query().Get("ref") != commitSHA || len(request.URL.Query()) != 1 {
				t.Errorf("contents query = %q", request.URL.RawQuery)
			}
			encoded := base64.StdEncoding.EncodeToString(wantContent)
			encoded = encoded[:20] + "\n" + encoded[20:]
			_, _ = fmt.Fprintf(writer, `{"type":"file","path":".omnigrex/team/developer.md","encoding":"base64","size":%d,"content":%q}`, len(wantContent), encoded)
		default:
			t.Errorf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatalf("NewAPIClient() error = %v", err)
	}
	commit, err := client.ResolveDefaultBranchCommit(context.Background(), "installation-token", "acme", "widgets")
	if err != nil {
		t.Fatalf("ResolveDefaultBranchCommit() error = %v", err)
	}
	if commit != commitSHA {
		t.Errorf("commit = %q, want %q", commit, commitSHA)
	}
	content, err := client.FetchRepositoryFile(context.Background(), "installation-token", "acme", "widgets", ".omnigrex/team/developer.md", commit)
	if err != nil {
		t.Fatalf("FetchRepositoryFile() error = %v", err)
	}
	if string(content) != string(wantContent) {
		t.Errorf("content = %q, want %q", content, wantContent)
	}
}

func TestAPIClientRejectsInvalidRepositoryContentRequestsWithoutHTTP(t *testing.T) {
	requests := 0
	client, err := githubapi.NewAPIClient(httpDoerFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("unexpected HTTP request")
	}), "https://api.github.test")
	if err != nil {
		t.Fatalf("NewAPIClient() error = %v", err)
	}
	tests := []struct {
		name   string
		path   string
		commit string
		cause  error
	}{
		{name: "empty path", commit: "abc123", cause: githubapi.ErrInvalidRepositoryPath},
		{name: "absolute path", path: "/developer.md", commit: "abc123", cause: githubapi.ErrInvalidRepositoryPath},
		{name: "parent traversal", path: "team/../developer.md", commit: "abc123", cause: githubapi.ErrInvalidRepositoryPath},
		{name: "empty commit", path: "developer.md", cause: githubapi.ErrInvalidCommitSHA},
		{name: "abbreviated commit", path: "developer.md", commit: "abc123", cause: githubapi.ErrInvalidCommitSHA},
		{name: "revision expression", path: "developer.md", commit: "main^{tree}", cause: githubapi.ErrInvalidCommitSHA},
		{name: "non-hex object", path: "developer.md", commit: "commit-sha", cause: githubapi.ErrInvalidCommitSHA},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := client.FetchRepositoryFile(context.Background(), "token", "acme", "widgets", test.path, test.commit)
			var configuration *githubapi.ConfigurationError
			if !errors.Is(err, test.cause) || !errors.As(err, &configuration) || !configuration.Permanent() {
				t.Errorf("error = %T %v, want ConfigurationError wrapping %v", err, err, test.cause)
			}
		})
	}
	if requests != 0 {
		t.Errorf("invalid requests made %d HTTP calls", requests)
	}
}

func TestAPIClientRejectsInvalidRepositoryResponses(t *testing.T) {
	const path = ".omnigrex/team/developer.md"
	const commit = "0123456789abcdef0123456789abcdef01234567"
	tests := []struct {
		name string
		body string
	}{
		{name: "directory response", body: `[{"type":"file"}]`},
		{name: "non-file type", body: `{"type":"symlink","path":".omnigrex/team/developer.md","encoding":"base64","size":1,"content":"eA=="}`},
		{name: "different path", body: `{"type":"file","path":".omnigrex/team/reviewer.md","encoding":"base64","size":1,"content":"eA=="}`},
		{name: "unsupported encoding", body: `{"type":"file","path":".omnigrex/team/developer.md","encoding":"utf-8","size":1,"content":"eA=="}`},
		{name: "negative size", body: `{"type":"file","path":".omnigrex/team/developer.md","encoding":"base64","size":-1,"content":""}`},
		{name: "declared size too large", body: fmt.Sprintf(`{"type":"file","path":"%s","encoding":"base64","size":%d,"content":""}`, path, githubapi.MaxRepositoryFileSize+1)},
		{name: "malformed base64", body: `{"type":"file","path":".omnigrex/team/developer.md","encoding":"base64","size":1,"content":"%%%"}`},
		{name: "unexpected base64 whitespace", body: `{"type":"file","path":".omnigrex/team/developer.md","encoding":"base64","size":1,"content":"e A=="}`},
		{name: "size mismatch", body: `{"type":"file","path":".omnigrex/team/developer.md","encoding":"base64","size":2,"content":"eA=="}`},
		{name: "encoded content over bound", body: fmt.Sprintf(`{"type":"file","path":"%s","encoding":"base64","size":1,"content":%q}`, path, base64.StdEncoding.EncodeToString(make([]byte, githubapi.MaxRepositoryFileSize+1)))},
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
			_, err = client.FetchRepositoryFile(context.Background(), "token", "acme", "widgets", path, commit)
			if !errors.Is(err, githubapi.ErrInvalidAPIResponse) {
				t.Errorf("error = %T %v, want ErrInvalidAPIResponse", err, err)
			}
		})
	}
}

func TestAPIClientRejectsInvalidDefaultBranchResolutionResponses(t *testing.T) {
	tests := []struct {
		name       string
		firstBody  string
		secondBody string
	}{
		{name: "missing default branch", firstBody: `{}`},
		{name: "blank default branch", firstBody: `{"default_branch":"  "}`},
		{name: "missing commit SHA", firstBody: `{"default_branch":"main"}`, secondBody: `{}`},
		{name: "unsafe commit SHA", firstBody: `{"default_branch":"main"}`, secondBody: `{"sha":"main~1"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				requests++
				if requests == 1 {
					_, _ = fmt.Fprint(writer, test.firstBody)
					return
				}
				_, _ = fmt.Fprint(writer, test.secondBody)
			}))
			defer server.Close()
			client, err := githubapi.NewAPIClient(server.Client(), server.URL)
			if err != nil {
				t.Fatalf("NewAPIClient() error = %v", err)
			}
			_, err = client.ResolveDefaultBranchCommit(context.Background(), "token", "acme", "widgets")
			if !errors.Is(err, githubapi.ErrInvalidAPIResponse) {
				t.Errorf("error = %T %v, want ErrInvalidAPIResponse", err, err)
			}
		})
	}
}

func TestAPIClientClassifiesRepositoryContentFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "/contents/") {
			writer.WriteHeader(http.StatusBadGateway)
			_, _ = fmt.Fprint(writer, `{"message":"upstream unavailable"}`)
			return
		}
		http.NotFound(writer, request)
	}))
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatalf("NewAPIClient() error = %v", err)
	}
	_, err = client.FetchRepositoryFile(context.Background(), "token", "acme", "widgets", ".omnigrex/team/developer.md", strings.Repeat("a", 40))
	var transient *githubapi.TransientError
	if !errors.As(err, &transient) || !transient.Transient() {
		t.Errorf("error = %T %v, want TransientError", err, err)
	}
}
